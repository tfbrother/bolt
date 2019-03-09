// +build !windows,!plan9,!solaris

package bolt

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// flock acquires an advisory lock on a file descriptor.
func flock(db *DB, mode os.FileMode, exclusive bool, timeout time.Duration) error {
	var t time.Time
	for {
		// If we're beyond our timeout then return an error.
		// This can only occur after we've attempted a flock once.
		if t.IsZero() {
			t = time.Now()
		} else if timeout > 0 && time.Since(t) > timeout {
			return ErrTimeout
		}
		flag := syscall.LOCK_SH
		if exclusive {
			flag = syscall.LOCK_EX
		}

		// Otherwise attempt to obtain an exclusive lock.
		err := syscall.Flock(int(db.file.Fd()), flag|syscall.LOCK_NB)
		if err == nil {
			return nil
		} else if err != syscall.EWOULDBLOCK {
			return err
		}

		// Wait for a bit and try again.
		time.Sleep(50 * time.Millisecond)
	}
}

// funlock releases an advisory lock on a file descriptor.
func funlock(db *DB) error {
	return syscall.Flock(int(db.file.Fd()), syscall.LOCK_UN)
}

// mmap memory maps a DB's data file.
func mmap(db *DB, sz int) error {
	// Map the data file to memory.
	b, err := syscall.Mmap(int(db.file.Fd()), 0, sz, syscall.PROT_READ, syscall.MAP_SHARED|db.MmapFlags)
	if err != nil {
		return err
	}

	// Advise the kernel that the mmap is accessed randomly.
	// 告诉内核，mmap会被随机访问
	if err := madvise(b, syscall.MADV_RANDOM); err != nil {
		return fmt.Errorf("madvise: %s", err)
	}

	// Save the original byte slice and convert to a byte array pointer.
	db.dataref = b
	// TODO 这个并不会申请[maxMapSize]byte大小的空间，但是使用data[pos]时，
	// 是不是意味着data[0:pos+1]空间会自动分配？return (*page)(unsafe.Pointer(&db.data[pos]))
	// 这是操作系统虚拟内存实现的，此处只是在虚拟地址空间里面申请了这么大的内存，实际上只有用到的时候才会
	// 真正分配。操作系统也是4k/页。[maxMapSize]byte这么大的内存，会分成很多个页，使用到某一页如果不在内存，
	// 产生缺页中断，此时才会真正申请内存。
	db.data = (*[maxMapSize]byte)(unsafe.Pointer(&b[0]))
	db.datasz = sz
	return nil
}

// munmap unmaps a DB's data file from memory.
func munmap(db *DB) error {
	// Ignore the unmap if we have no mapped data.
	if db.dataref == nil {
		return nil
	}

	// Unmap using the original byte slice.
	err := syscall.Munmap(db.dataref)
	db.dataref = nil
	db.data = nil
	db.datasz = 0
	return err
}

// NOTE: This function is copied from stdlib because it is not available on darwin.
// https://blog.csdn.net/hustmp/article/details/8331961
func madvise(b []byte, advice int) (err error) {
	// 建议内核，在从 &b[0] 指定的地址开始，长度等于 len(b) 参数值的范围内，该区域的用户虚拟内存应遵循特定的使用模式。
	// 内核使用这些信息优化与指定范围关联的资源的处理和维护过程。
	_, _, e1 := syscall.Syscall(syscall.SYS_MADVISE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(advice))
	if e1 != 0 {
		err = e1
	}
	return
}
