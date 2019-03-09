package bolt

import (
	"bytes"
	"fmt"
	"sort"
	"unsafe"
)

// node represents an in-memory, deserialized page.
type node struct {
	bucket     *Bucket
	isLeaf     bool // 标记该节点是否为叶子节点，决定inode中记录的是什么
	unbalanced bool // 当该node上有删除操作时，标记为true，当Tx执行Commit时，会执行rebalance，将inode重新排列
	spilled    bool
	key        []byte // 当加载page变成node缓存时，将下边界inode[0]的key缓存在node上，用于在parent node 查找本身时使用
	pgid       pgid
	parent     *node // Bucket的rootNode.parent == nil
	children   nodes // children记录通过node访问过的子node，还包含rebalance/spill过程中增加的子node
	inodes     inodes
}

// root returns the top-level node this node is attached to.
func (n *node) root() *node {
	if n.parent == nil {
		return n
	}
	return n.parent.root()
}

// minKeys returns the minimum number of inodes this node should have.
func (n *node) minKeys() int {
	if n.isLeaf {
		return 1
	}
	return 2
}

// size returns the size of the node after serialization.
func (n *node) size() int {
	sz, elsz := pageHeaderSize, n.pageElementSize()
	for i := 0; i < len(n.inodes); i++ {
		item := &n.inodes[i]
		sz += elsz + len(item.key) + len(item.value)
	}
	return sz
}

// sizeLessThan returns true if the node is less than a given size.
// This is an optimization to avoid calculating a large node when we only need
// to know if it fits inside a certain page size.
func (n *node) sizeLessThan(v int) bool {
	sz, elsz := pageHeaderSize, n.pageElementSize()
	for i := 0; i < len(n.inodes); i++ {
		item := &n.inodes[i]
		sz += elsz + len(item.key) + len(item.value)
		if sz >= v {
			return false
		}
	}
	return true
}

// pageElementSize returns the size of each page element based on the type of node.
func (n *node) pageElementSize() int {
	if n.isLeaf {
		return leafPageElementSize
	}
	return branchPageElementSize
}

// childAt returns the child node at a given index.
func (n *node) childAt(index int) *node {
	if n.isLeaf {
		panic(fmt.Sprintf("invalid childAt(%d) on a leaf node", index))
	}
	return n.bucket.node(n.inodes[index].pgid, n)
}

// childIndex returns the index of a given child node.
func (n *node) childIndex(child *node) int {
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, child.key) != -1 })
	return index
}

// numChildren returns the number of children.
func (n *node) numChildren() int {
	return len(n.inodes)
}

// nextSibling returns the next node with the same parent.
func (n *node) nextSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index >= n.parent.numChildren()-1 {
		return nil
	}
	return n.parent.childAt(index + 1)
}

// prevSibling returns the previous node with the same parent.
func (n *node) prevSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index == 0 {
		return nil
	}
	return n.parent.childAt(index - 1)
}

// put inserts a key/value.
// 在node中插入一个key/value对，即inode
func (n *node) put(oldKey, newKey, value []byte, pgid pgid, flags uint32) {
	if pgid >= n.bucket.tx.meta.pgid {
		panic(fmt.Sprintf("pgid (%d) above high water mark (%d)", pgid, n.bucket.tx.meta.pgid))
	} else if len(oldKey) <= 0 {
		panic("put: zero-length old key")
	} else if len(newKey) <= 0 {
		panic("put: zero-length new key")
	}

	// Find insertion index.
	// 其实是在n.inodes中找到第一个大于等于oldKey的索引位置。B树的特性
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, oldKey) != -1 })

	// Add capacity and shift nodes if we don't have an exact match and need to insert.
	// 如果oldKey没有找到精确的匹配，则就是插入，否则就是替换
	exact := (len(n.inodes) > 0 && index < len(n.inodes) && bytes.Equal(n.inodes[index].key, oldKey))
	if !exact { // 在index处进行插入
		n.inodes = append(n.inodes, inode{})
		copy(n.inodes[index+1:], n.inodes[index:])
	}

	inode := &n.inodes[index]
	inode.flags = flags
	inode.key = newKey
	inode.value = value
	inode.pgid = pgid
	_assert(len(inode.key) > 0, "put: zero-length inode key")
}

// del removes a key from the node.
// 删除node.inodes[index].key == key的inode
func (n *node) del(key []byte) {
	// Find index of key.
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, key) != -1 })

	// Exit if the key isn't found.
	if index >= len(n.inodes) || !bytes.Equal(n.inodes[index].key, key) {
		return
	}

	// Delete inode from the node.
	n.inodes = append(n.inodes[:index], n.inodes[index+1:]...)

	// Mark the node as needing rebalancing.
	n.unbalanced = true
}

// read initializes the node from a page.
// 将磁盘中的page加载到内存中的node来
func (n *node) read(p *page) {
	n.pgid = p.id
	n.isLeaf = ((p.flags & leafPageFlag) != 0)
	n.inodes = make(inodes, int(p.count))

	for i := 0; i < int(p.count); i++ {
		inode := &n.inodes[i]
		if n.isLeaf {
			elem := p.leafPageElement(uint16(i))
			inode.flags = elem.flags
			inode.key = elem.key()
			inode.value = elem.value()
		} else {
			elem := p.branchPageElement(uint16(i))
			inode.pgid = elem.pgid
			inode.key = elem.key()
		}
		_assert(len(inode.key) > 0, "read: zero-length inode key")
	}

	// Save first key so we can find the node in the parent when we spill.
	if len(n.inodes) > 0 {
		n.key = n.inodes[0].key
		_assert(len(n.key) > 0, "read: zero-length node key")
	} else {
		n.key = nil
	}
}

// write writes the items onto one or more pages.
// 将node从内存写入到指定的磁盘page中
func (n *node) write(p *page) {
	// Initialize page.
	if n.isLeaf {
		p.flags |= leafPageFlag
	} else {
		p.flags |= branchPageFlag
	}

	// 0xFFFF是16位的最大值，而p.count是uint16位的
	if len(n.inodes) >= 0xFFFF {
		panic(fmt.Sprintf("inode overflow: %d (pgid=%d)", len(n.inodes), p.id))
	}
	p.count = uint16(len(n.inodes))

	// Stop here if there are no items to write.
	if p.count == 0 {
		return
	}

	// Loop over each item and write it to the page.
	b := (*[maxAllocSize]byte)(unsafe.Pointer(&p.ptr))[n.pageElementSize()*len(n.inodes):]
	for i, item := range n.inodes {
		_assert(len(item.key) > 0, "write: zero-length inode key")

		// Write the page element.
		if n.isLeaf {
			elem := p.leafPageElement(uint16(i))
			elem.pos = uint32(uintptr(unsafe.Pointer(&b[0])) - uintptr(unsafe.Pointer(elem)))
			elem.flags = item.flags
			elem.ksize = uint32(len(item.key))
			elem.vsize = uint32(len(item.value))
		} else {
			elem := p.branchPageElement(uint16(i))
			elem.pos = uint32(uintptr(unsafe.Pointer(&b[0])) - uintptr(unsafe.Pointer(elem)))
			elem.ksize = uint32(len(item.key))
			elem.pgid = item.pgid
			_assert(elem.pgid != p.id, "write: circular dependency occurred")
		}

		// If the length of key+value is larger than the max allocation size
		// then we need to reallocate the byte array pointer.
		//
		// See: https://github.com/boltdb/bolt/pull/335
		klen, vlen := len(item.key), len(item.value)
		if len(b) < klen+vlen {
			b = (*[maxAllocSize]byte)(unsafe.Pointer(&b[0]))[:]
		}

		// Write data for the element to the end of the page.
		copy(b[0:], item.key)
		b = b[klen:]
		copy(b[0:], item.value)
		b = b[vlen:]
	}

	// DEBUG ONLY: n.dump()
}

// split breaks up a node into multiple smaller nodes, if appropriate.
// This should only be called from the spill() function.
// 实际上就是把node分成两段，其中一段满足node要求的大小，另一段再进一步按相同规则分成两段，一直到不能再分为止。
func (n *node) split(pageSize int) []*node {
	var nodes []*node

	node := n
	for {
		// Split node into two.
		// 第一个结点满足条件，第二个结点会被再次调用进行分裂成两个结点
		a, b := node.splitTwo(pageSize)
		nodes = append(nodes, a)

		// If we can't split then exit the loop.
		if b == nil {
			break
		}

		// Set node to b so it gets split on the next iteration.
		node = b
	}

	return nodes
}

// splitTwo breaks up a node into two smaller nodes, if appropriate.
// This should only be called from the split() function.
// 将一个结点分裂成两个结点
func (n *node) splitTwo(pageSize int) (*node, *node) {
	// Ignore the split if the page doesn't have at least enough nodes for
	// two pages or if the nodes can fit in a single page.
	// 节点分裂的条件:
	// 1) 节点大小超过了页大小， 且
	// 2) 节点Key个数大于每节点Key数量最小值的两倍，这是为了保证分裂出的两个节点中的Key数量都大于每节点Key数量的最小值;
	if len(n.inodes) <= (minKeysPerPage*2) || n.sizeLessThan(pageSize) {
		return n, nil
	}

	// Determine the threshold before starting a new node.
	var fillPercent = n.bucket.FillPercent
	if fillPercent < minFillPercent {
		fillPercent = minFillPercent
	} else if fillPercent > maxFillPercent {
		fillPercent = maxFillPercent
	}
	// 决定分裂的门限值，即页大小 x 填充率;
	threshold := int(float64(pageSize) * fillPercent)

	// Determine split position and sizes of the two pages.
	// 计算分裂的位置
	splitIndex, _ := n.splitIndex(threshold)

	// Split node into two separate nodes.
	// If there's no parent then we'll need to create one.
	// 如果要分裂的节点没有父节点(可能是根节点)，则应该新建一个父node，同时将当前节点设为它的子node;
	if n.parent == nil {
		n.parent = &node{bucket: n.bucket, children: []*node{n}}
	}

	// Create a new node and add it to the parent.
	// 创建了一个新node，并将当前node的父节点设为它的父节点;
	next := &node{bucket: n.bucket, isLeaf: n.isLeaf, parent: n.parent}
	n.parent.children = append(n.parent.children, next)

	// Split inodes across two nodes.
	// 将当前node的从分裂位置开始的右半部分记录拷贝给新node;
	next.inodes = n.inodes[splitIndex:]
	//将当前node的记录更新为原记录集合从分裂位置开始的左半部分，从而实现了将当前node一分为二;
	n.inodes = n.inodes[:splitIndex]

	// Update the statistics.
	n.bucket.tx.stats.Split++

	return n, next
}

// splitIndex finds the position where a page will fill a given threshold.
// It returns the index as well as the size of the first page.
// This is only be called from split().
// 计算分裂的索引
func (n *node) splitIndex(threshold int) (index, sz int) {
	sz = pageHeaderSize

	// Loop until we only have the minimum number of keys required for the second page.
	for i := 0; i < len(n.inodes)-minKeysPerPage; i++ {
		index = i
		inode := n.inodes[i]
		elsize := n.pageElementSize() + len(inode.key) + len(inode.value)

		// If we have at least the minimum number of keys and adding another
		// node would put us over the threshold then exit and return.
		if i >= minKeysPerPage && sz+elsize > threshold {
			break
		}

		// Add the element size to the total size.
		sz += elsize
	}

	return
}

// spill writes the nodes to dirty pages and splits nodes as it goes.
// Returns an error if dirty pages cannot be allocated.
func (n *node) spill() error {
	var tx = n.bucket.tx
	if n.spilled {
		return nil
	}

	// Spill child nodes first. Child nodes can materialize sibling nodes in
	// the case of split-merge so we cannot use a range loop. We have to check
	// the children size on every loop iteration.
	// 子节点进行深度优先访问并递归调用spill()，需要注意的是，子节点可能会分裂成多个节点，分裂出来的新节点也是当前节点的子节点，
	// n.children这个slice的size会在循环中变化，所以不能使用rang的方式循环访问；同时，分裂出来的新节点会在后面代码处被设为spilled，
	// 所以在下一次循环访问到新的子节点时不会重新spill，这也是上面对spilled进行检查的原因;
	sort.Sort(n.children)
	for i := 0; i < len(n.children); i++ {
		if err := n.children[i].spill(); err != nil {
			return err
		}
	}

	// We no longer need the child list because it's only used for spill tracking.
	//当所有子节点spill完成后，将子节点引用集合children置为空，以防向上递归调用spill的时候形成回路;
	n.children = nil

	// Split nodes into appropriate sizes. The first node will always be n.
	// 如果n不能被split，nodes就会为[]*node{n}
	// TODO 当nodes == []*node{n} 这种特殊情况出现的时候，会不会出现异常？
	// 调用node的split()方法按页大小将node分裂出若干新node，新node与当前node共享同一个父node，返回的nodes中包含当前node;
	var nodes = n.split(tx.db.pageSize)
	for _, node := range nodes {
		// Add node's page to the freelist if it's not new.
		// 释放当前node的所占页，因为随后要为它分配新的页，我们前面说过tx commit是只会向磁盘写入当前tx分配的脏页，
		// 所以这里要对当前node重新分配页;
		if node.pgid > 0 {
			tx.db.freelist.free(tx.meta.txid, tx.page(node.pgid))
			node.pgid = 0
		}

		// Allocate contiguous space for the node.
		// 调用Tx的allocate()方法为分裂产生的node分配页缓存，请注意，通过splite()方法分裂node后，node的大小为页大小 * 填充率，
		// 默认填充率为50%，而且一般地它的值小于100%，所以这里为每个node实际上是分配一个页框;
		p, err := tx.allocate((node.size() / tx.db.pageSize) + 1)
		if err != nil {
			return err
		}

		// Write the node.
		if p.id >= tx.meta.pgid {
			panic(fmt.Sprintf("pgid (%d) above high water mark (%d)", p.id, tx.meta.pgid))
		}
		// 将新node的页号设为分配给他的页框的页号，同时将新node序列化并写入刚刚分配的页缓存;
		node.pgid = p.id
		node.write(p)
		node.spilled = true

		// Insert into parent inodes.
		if node.parent != nil {
			var key = node.key
			if key == nil {
				key = node.inodes[0].key
			}

			// TODO value=nil,分支结点保存的是索引，不保存值，flags是0
			// 向父节点更新或添加Key和Pointer，以指向分裂产生的新node。
			node.parent.put(key, node.inodes[0].key, nil, node.pgid, 0)
			// 将父node的key设为第一个子node的第一个key;
			node.key = node.inodes[0].key
			_assert(len(node.key) > 0, "spill: zero-length node key")
		}

		// Update the statistics.
		tx.stats.Spill++
	}

	// If the root node split and created a new root then we need to spill that
	// as well. We'll clear out the children to make sure it doesn't try to respill.
	// 从根节点处递归完所有子节点的spill过程后，若根节点需要分裂，则它分裂后将产生新的根节点，
	// 因此需要对新产生的根节点进行spill;
	// TODO 上面是递归children[i].spill()，此处又parent.spill()，是否可能存在死循环的情况？
	// 或者同一个结点多次被spill的情况？
	if n.parent != nil && n.parent.pgid == 0 {
		n.children = nil
		return n.parent.spill()
	}

	return nil
}

// rebalance attempts to combine the node with sibling nodes if the node fill
// size is below a threshold or if there are not enough keys.
// 节点的rebalance也是一个递归的过程，它会从当前结点一直进行到根节点处;
func (n *node) rebalance() {
	// 只有当节点中有过删除操作时，unbalanced才为true;
	if !n.unbalanced {
		return
	}
	n.unbalanced = false

	// Update statistics.
	n.bucket.tx.stats.Rebalance++

	// Ignore if node is above threshold (25%) and has enough keys.
	// 只有当节点存的K/V总大小小于页大小的25%且节点中Key的数量少于设定的每节点Key数量最小值时，才会进行再平衡;
	var threshold = n.bucket.tx.db.pageSize / 4
	if n.size() > threshold && len(n.inodes) > n.minKeys() {
		return
	}

	// Root node has special handling.
	// 只有根节点Bucket.rootNode有n.parent == nil，每个Bucket都有一个rootNode
	if n.parent == nil {
		// If root node is a branch and only has one node then collapse it.
		// 根节点只有一个子节点的情形:只能向上合并
		// 将子节点上的inodes拷贝到根节点上，并将子节点的所有孩子移交给根节点，并将孩子节点的父节点更新为根节点；
		if !n.isLeaf && len(n.inodes) == 1 {
			// Move root's child up.
			child := n.bucket.node(n.inodes[0].pgid, n)
			n.isLeaf = child.isLeaf
			n.inodes = child.inodes[:]
			n.children = child.children

			// Reparent all child nodes being moved.
			// 为何没有递归的子结点进行rebalance呢
			for _, inode := range n.inodes {
				// TODO 上面也有一个child变量，难道此child的作用域只在for循环内？是的
				if child, ok := n.bucket.nodes[inode.pgid]; ok {
					child.parent = n
				}
			}

			// Remove old child.
			child.parent = nil
			delete(n.bucket.nodes, child.pgid)
			child.free()
		} else {
			// do nothing
			// TODO B+树的性质：根节点的下界是拥有两个子结点则不用进行rebalance。
		}

		return
	}

	// If node has no keys then just remove it.
	// 如果节点变成一个空节点，则将它从B+Tree中删除，并把父节点上的Key和Pointer删除，由于父节点上有删除，得对父节点进行再平衡;
	if n.numChildren() == 0 {
		// 删除n.parent.inodes中对应的inode
		n.parent.del(n.key)
		// 删除n.parent.children中对应的缓存node
		n.parent.removeChild(n)
		// 删除n.bucket.nodes中对应的缓存node
		delete(n.bucket.nodes, n.pgid)
		// 将n所在的页标记为free
		n.free()
		n.parent.rebalance()
		return
	}

	// TODO 为何此处要这样检查呢？
	_assert(n.parent.numChildren() > 1, "parent must have at least 2 children")

	// Destination node is right sibling if idx == 0, otherwise left sibling.
	// 决定是合并左节点还是右节点：
	// 	1.如果当前节点是父节点的第一个孩子，则将右节点中的记录合并到当前节点中；
	// 	2.如果当前节点是父节点的第二个或以上的节点，则将当前节点中的记录合并到左节点中;
	var target *node
	var useNextSibling = (n.parent.childIndex(n) == 0)
	if useNextSibling {
		target = n.nextSibling()
	} else {
		target = n.prevSibling()
	}

	// If both this node and the target node are too small then merge them.
	// 右节点中记录合并到当前节点:
	// 	1.首先将右节点的孩子节点全部变成当前节点的孩子，右节点将所有孩子移除；
	// 	2.随后，将右节点中的记录全部拷贝到当前节点；
	// 	3.最后，将右节点从B+Tree中移除，并将父节点中与右节点对应的记录删除;
	if useNextSibling {
		// Reparent all child nodes being moved.
		for _, inode := range target.inodes {
			// TODO 如果inode.pgid没有缓存在bucket.nodes下面就不处理？？不会有逻辑问题？？？
			if child, ok := n.bucket.nodes[inode.pgid]; ok {
				child.parent.removeChild(child)
				child.parent = n
				child.parent.children = append(child.parent.children, child)
			}
		}

		// Copy over inodes from target and remove target.
		n.inodes = append(n.inodes, target.inodes...)
		n.parent.del(target.key)
		n.parent.removeChild(target)
		delete(n.bucket.nodes, target.pgid)
		target.free()
	} else { // 将当前节点中的记录合并到左节点
		// Reparent all child nodes being moved.
		for _, inode := range n.inodes {
			if child, ok := n.bucket.nodes[inode.pgid]; ok {
				child.parent.removeChild(child)
				child.parent = target
				child.parent.children = append(child.parent.children, child)
			}
		}

		// Copy over inodes to target and remove node.
		target.inodes = append(target.inodes, n.inodes...)
		n.parent.del(n.key)
		n.parent.removeChild(n)
		delete(n.bucket.nodes, n.pgid)
		n.free()
	}

	// Either this node or the target node was deleted from the parent so rebalance it.
	// 合并兄弟节点与当前节点时，会移除一个节点并从父节点中删除一个记录，
	// 所以需要对父节点进行再平衡(节点的rebalance也是一个递归的过程，它会从当前结点一直进行到根节点处）
	n.parent.rebalance()
}

// removes a node from the list of in-memory children.
// This does not affect the inodes.
func (n *node) removeChild(target *node) {
	for i, child := range n.children {
		if child == target {
			n.children = append(n.children[:i], n.children[i+1:]...)
			return
		}
	}
}

// dereference causes the node to copy all its inode key/value references to heap memory.
// This is required when the mmap is reallocated so inodes are not pointing to stale data.
func (n *node) dereference() {
	if n.key != nil {
		key := make([]byte, len(n.key))
		copy(key, n.key)
		n.key = key
		_assert(n.pgid == 0 || len(n.key) > 0, "dereference: zero-length node key on existing node")
	}

	for i := range n.inodes {
		inode := &n.inodes[i]

		key := make([]byte, len(inode.key))
		copy(key, inode.key)
		inode.key = key
		_assert(len(inode.key) > 0, "dereference: zero-length inode key")

		value := make([]byte, len(inode.value))
		copy(value, inode.value)
		inode.value = value
	}

	// Recursively dereference children.
	for _, child := range n.children {
		child.dereference()
	}

	// Update statistics.
	n.bucket.tx.stats.NodeDeref++
}

// free adds the node's underlying page to the freelist.
func (n *node) free() {
	if n.pgid != 0 {
		n.bucket.tx.db.freelist.free(n.bucket.tx.meta.txid, n.bucket.tx.page(n.pgid))
		n.pgid = 0
	}
}

// dump writes the contents of the node to STDERR for debugging purposes.
/*
func (n *node) dump() {
	// Write node header.
	var typ = "branch"
	if n.isLeaf {
		typ = "leaf"
	}
	warnf("[NODE %d {type=%s count=%d}]", n.pgid, typ, len(n.inodes))

	// Write out abbreviated version of each item.
	for _, item := range n.inodes {
		if n.isLeaf {
			if item.flags&bucketLeafFlag != 0 {
				bucket := (*bucket)(unsafe.Pointer(&item.value[0]))
				warnf("+L %08x -> (bucket root=%d)", trunc(item.key, 4), bucket.root)
			} else {
				warnf("+L %08x -> %08x", trunc(item.key, 4), trunc(item.value, 4))
			}
		} else {
			warnf("+B %08x -> pgid=%d", trunc(item.key, 4), item.pgid)
		}
	}
	warn("")
}
*/

type nodes []*node

func (s nodes) Len() int           { return len(s) }
func (s nodes) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s nodes) Less(i, j int) bool { return bytes.Compare(s[i].inodes[0].key, s[j].inodes[0].key) == -1 }

// inode represents an internal node inside of a node.
// It can be used to point to elements in a page or point
// to an element which hasn't been added to a page yet.
type inode struct {
	// 当所在node为叶子节点时，记录key的flag
	flags uint32
	// 当所在node为叶子节点时，不使用，当所在node为分支节点时，记录所指向的page-id
	pgid pgid

	// 当所在node为叶子节点时，记录的为拥有的key；当所在node为分支节点时，记录的是子
	// 节点的上key的下边界。例如，当当前node为分支节点，拥有3个分支，[1,2,3][5,6,7][10,11,12]
	// 这该node上可能有3个inode，记录的key为[1,5,10]。当进行查找时2时，2<5,则去第0个子分支上继
	// 续搜索，当进行查找4时，1<4<5,则去第1个分支上去继续查找。
	key []byte
	// 当所在node为叶子节点时，记录的为拥有的value
	value []byte
}

type inodes []inode
