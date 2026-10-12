// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd
// +build darwin dragonfly freebsd linux netbsd openbsd

package storage

import (
	"errors"
	"os"
	"syscall"
)

type unixFileLock struct {
	f *os.File
}

func (fl *unixFileLock) release() error {
	if err := setFileLock(fl.f, false, false); err != nil {
		return err
	}
	return fl.f.Close()
}

func newFileLock(path string, readOnly bool) (fl fileLock, err error) {
	var flag int
	if readOnly {
		flag = os.O_RDONLY
	} else {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0) //nolint:gosec // path is inside the database directory the caller opened
	if os.IsNotExist(err) {
		f, err = os.OpenFile(path, flag|os.O_CREATE, 0644) //nolint:gosec // world files stay readable by other tools such as BDS
	}
	if err != nil {
		return
	}
	err = setFileLock(f, readOnly, true)
	if err != nil {
		_ = f.Close()
		return
	}
	fl = &unixFileLock{f: f}
	return
}

func setFileLock(f *os.File, readOnly, lock bool) error {
	how := syscall.LOCK_UN
	if lock {
		if readOnly {
			how = syscall.LOCK_SH
		} else {
			how = syscall.LOCK_EX
		}
	}
	return syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
}

func rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func isErrInvalid(err error) bool {
	// errors.Is also unwraps the *os.SyscallError and *os.PathError that
	// older Go versions returned around EINVAL.
	return errors.Is(err, os.ErrInvalid) || errors.Is(err, syscall.EINVAL)
}

func syncDir(name string) error {
	// As per fsync manpage, Linux seems to expect fsync on directory, however
	// some system don't support this, so we will ignore syscall.EINVAL.
	//
	// From fsync(2):
	//   Calling fsync() does not necessarily ensure that the entry in the
	//   directory containing the file has also reached disk. For that an
	//   explicit fsync() on a file descriptor for the directory is also needed.
	f, err := os.Open(name) //nolint:gosec // name is the database directory the caller opened
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil && !isErrInvalid(err) {
		return err
	}
	return nil
}
