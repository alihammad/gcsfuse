// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lease

import (
	"fmt"
	"io"
	"log"
	"sync"

	"golang.org/x/net/context"
)

// A function used by read proxies to refresh their contents. See notes on
// NewReadProxy.
type RefreshContentsFunc func(context.Context) (io.ReadCloser, error)

// Create a read proxy.
//
// The supplied function will be used to obtain the proxy's contents, the first
// time they're needed and whenever the supplied file leaser decides to expire
// the temporary copy thus obtained. It must return the same contents every
// time, and the contents must be of the given size.
func NewReadProxy(
	fl FileLeaser,
	size int64,
	refresh RefreshContentsFunc) (rl ReadLease) {
	rl = &autoRefreshingReadLease{
		leaser:  fl,
		size:    size,
		refresh: refresh,
	}

	return
}

// A wrapper around a read lease, exposing a similar interface with the
// following differences:
//
//  *  Contents are fetched and re-fetched automatically when needed. Therefore
//     the user need not worry about lease expiration.
//
//  *  Methods that may involve fetching the contents (reading, seeking) accept
//     context arguments, so as to be cancellable.
//
type ReadProxy struct {
	mu sync.Mutex

	/////////////////////////
	// Constant data
	/////////////////////////

	size int64

	/////////////////////////
	// Dependencies
	/////////////////////////

	leaser FileLeaser
	f      func() (io.ReadCloser, error)

	/////////////////////////
	// Mutable state
	/////////////////////////

	// Set to true when we've been revoked for good.
	//
	// GUARDED_BY(mu)
	revoked bool

	// The current wrapped lease, or nil if one has never been issued.
	//
	// GUARDED_BY(mu)
	wrapped ReadLease
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

// Attempt to clean up after the supplied read/write lease.
func destroyReadWriteLease(rwl ReadWriteLease) {
	var err error
	defer func() {
		if err != nil {
			log.Printf("Error destroying read/write lease: %v", err)
		}
	}()

	// Downgrade to a read lease.
	rl, err := rwl.Downgrade()
	if err != nil {
		err = fmt.Errorf("Downgrade: %v", err)
		return
	}

	// Revoke the read lease.
	rl.Revoke()
}

func isRevokedErr(err error) bool {
	_, ok := err.(*RevokedError)
	return ok
}

// Set up a read/write lease and fill in our contents.
//
// REQUIRES: The caller has observed that rl.lease has expired.
//
// LOCKS_REQUIRED(rl.mu)
func (rl *autoRefreshingReadLease) getContents() (
	rwl ReadWriteLease, err error) {
	// Obtain some space to write the contents.
	rwl, err = rl.leaser.NewFile()
	if err != nil {
		err = fmt.Errorf("NewFile: %v", err)
		return
	}

	// Attempt to clean up if we exit early.
	defer func() {
		if err != nil {
			destroyReadWriteLease(rwl)
		}
	}()

	// Obtain the reader for our contents.
	rc, err := rl.f()
	if err != nil {
		err = fmt.Errorf("User function: %v", err)
		return
	}

	defer func() {
		closeErr := rc.Close()
		if closeErr != nil && err == nil {
			err = fmt.Errorf("Close: %v", closeErr)
		}
	}()

	// Copy into the read/write lease.
	copied, err := io.Copy(rwl, rc)
	if err != nil {
		err = fmt.Errorf("Copy: %v", err)
		return
	}

	// Did the user lie about the size?
	if copied != rl.Size() {
		err = fmt.Errorf("Copied %v bytes; expected %v", copied, rl.Size())
		return
	}

	return
}

// Downgrade and save the supplied read/write lease obtained with getContents
// for later use.
//
// LOCKS_REQUIRED(rl.mu)
func (rl *autoRefreshingReadLease) saveContents(rwl ReadWriteLease) {
	downgraded, err := rwl.Downgrade()
	if err != nil {
		log.Printf("Failed to downgrade write lease (%q); abandoning.", err.Error())
		return
	}

	rl.wrapped = downgraded
}

////////////////////////////////////////////////////////////////////////
// Public interface
////////////////////////////////////////////////////////////////////////

// Semantics matching io.Reader, except with context support.
func (rl *autoRefreshingReadLease) Read(
	ctx context.Context,
	p []byte) (n int, err error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Special case: have we been permanently revoked?
	if rl.revoked {
		err = &RevokedError{}
		return
	}

	// Common case: is the existing lease still valid?
	if rl.wrapped != nil {
		n, err = rl.wrapped.Read(p)
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Get hold of a read/write lease containing our contents.
	rwl, err := rl.getContents()
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	defer rl.saveContents(rwl)

	// Serve from the read/write lease.
	n, err = rwl.Read(p)

	return
}

// Semantics matching io.Seeker, except with context support.
func (rl *autoRefreshingReadLease) Seek(
	ctx context.Context,
	offset int64,
	whence int) (off int64, err error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Special case: have we been permanently revoked?
	if rl.revoked {
		err = &RevokedError{}
		return
	}

	// Common case: is the existing lease still valid?
	if rl.wrapped != nil {
		off, err = rl.wrapped.Seek(offset, whence)
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Get hold of a read/write lease containing our contents.
	rwl, err := rl.getContents()
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	defer rl.saveContents(rwl)

	// Serve from the read/write lease.
	off, err = rwl.Seek(offset, whence)

	return
}

// Semantics matching io.ReaderAt, except with context support.
func (rl *autoRefreshingReadLease) ReadAt(
	ctx context.Context,
	p []byte,
	off int64) (n int, err error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Special case: have we been permanently revoked?
	if rl.revoked {
		err = &RevokedError{}
		return
	}

	// Common case: is the existing lease still valid?
	if rl.wrapped != nil {
		n, err = rl.wrapped.ReadAt(p, off)
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Get hold of a read/write lease containing our contents.
	rwl, err := rl.getContents()
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	defer rl.saveContents(rwl)

	// Serve from the read/write lease.
	n, err = rwl.ReadAt(p, off)

	return
}

// Return the size of the proxied content. Guarantees to not block.
func (rl *autoRefreshingReadLease) Size() (size int64) {
	size = rl.size
	return
}

// For testing use only; do not touch.
func (rl *autoRefreshingReadLease) Destroyed() (destroyed bool) {
	panic("TODO")
}

// Return a read/write lease for the proxied contents. The read proxy must not
// be used after calling this method.
func (rl *autoRefreshingReadLease) Upgrade() (rwl ReadWriteLease, err error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Special case: have we been permanently revoked?
	if rl.revoked {
		err = &RevokedError{}
		return
	}

	// If we succeed, we are now revoked.
	defer func() {
		if err == nil {
			rl.revoked = true
		}
	}()

	// Common case: is the existing lease still valid?
	if rl.wrapped != nil {
		rwl, err = rl.wrapped.Upgrade()
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Build the read/write lease anew.
	rwl, err = rl.getContents()
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	return
}

// Destroy any resources in use by the read proxy. It must not be used further.
func (rl *autoRefreshingReadLease) Destroy() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.revoked = true
	if rl.wrapped != nil {
		rl.wrapped.Revoke()
	}
}
