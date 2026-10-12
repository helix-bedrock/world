// Copyright (c) 2014, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package testutil

import (
	"fmt"
	"math/rand"

	"github.com/onsi/gomega"

	"github.com/helix-bedrock/world/leveldb/iterator"
)

// IterAct is an iterator movement IteratorTesting can perform.
type IterAct int

func (a IterAct) String() string {
	switch a {
	case IterNone:
		return "none"
	case IterFirst:
		return "first"
	case IterLast:
		return "last"
	case IterPrev:
		return "prev"
	case IterNext:
		return "next"
	case IterSeek:
		return "seek"
	case IterSOI:
		return "soi"
	case IterEOI:
		return "eoi"
	}
	return "unknown"
}

// Iterator actions.
const (
	IterNone IterAct = iota
	IterFirst
	IterLast
	IterPrev
	IterNext
	IterSeek
	IterSOI
	IterEOI
)

// IteratorTesting checks an iterator against an in-memory model.
type IteratorTesting struct {
	KeyValue
	Iter         iterator.Iterator
	Rand         *rand.Rand
	PostFn       func(t *IteratorTesting)
	Pos          int
	Act, LastAct IterAct

	once bool
}

func (t *IteratorTesting) init() {
	if !t.once {
		t.Pos = -1
		t.once = true
	}
}

func (t *IteratorTesting) post() {
	if t.PostFn != nil {
		t.PostFn(t)
	}
}

func (t *IteratorTesting) setAct(act IterAct) {
	t.LastAct, t.Act = t.Act, act
}

func (t *IteratorTesting) text() string {
	return fmt.Sprintf("at pos %d and last action was <%v> -> <%v>", t.Pos, t.LastAct, t.Act)
}

// Text describes the last action, for failure messages.
func (t *IteratorTesting) Text() string {
	return "IteratorTesting is " + t.text()
}

// IsFirst reports whether the iterator is on the first entry.
func (t *IteratorTesting) IsFirst() bool {
	t.init()
	return t.Len() > 0 && t.Pos == 0
}

// IsLast reports whether the iterator is on the last entry.
func (t *IteratorTesting) IsLast() bool {
	t.init()
	return t.Len() > 0 && t.Pos == t.Len()-1
}

// TestKV checks the entry the iterator is on.
func (t *IteratorTesting) TestKV() {
	t.init()
	key, value := t.Index(t.Pos)
	gomega.Expect(t.Iter.Key()).NotTo(gomega.BeNil())
	gomega.Expect(t.Iter.Key()).Should(gomega.Equal(key), "Key is invalid, %s", t.text())
	gomega.Expect(t.Iter.Value()).Should(gomega.Equal(value), "Value for key %q, %s", key, t.text())
}

// First moves to the first entry and checks it.
func (t *IteratorTesting) First() {
	t.init()
	t.setAct(IterFirst)

	ok := t.Iter.First()
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	if t.Len() > 0 {
		t.Pos = 0
		gomega.Expect(ok).Should(gomega.BeTrue(), t.Text())
		t.TestKV()
	} else {
		t.Pos = -1
		gomega.Expect(ok).ShouldNot(gomega.BeTrue(), t.Text())
	}
	t.post()
}

// Last moves to the last entry and checks it.
func (t *IteratorTesting) Last() {
	t.init()
	t.setAct(IterLast)

	ok := t.Iter.Last()
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	if t.Len() > 0 {
		t.Pos = t.Len() - 1
		gomega.Expect(ok).Should(gomega.BeTrue(), t.Text())
		t.TestKV()
	} else {
		t.Pos = 0
		gomega.Expect(ok).ShouldNot(gomega.BeTrue(), t.Text())
	}
	t.post()
}

// Next moves forward and checks the entry.
func (t *IteratorTesting) Next() {
	t.init()
	t.setAct(IterNext)

	ok := t.Iter.Next()
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	if t.Pos < t.Len()-1 {
		t.Pos++
		gomega.Expect(ok).Should(gomega.BeTrue(), t.Text())
		t.TestKV()
	} else {
		t.Pos = t.Len()
		gomega.Expect(ok).ShouldNot(gomega.BeTrue(), t.Text())
	}
	t.post()
}

// Prev moves backward and checks the entry.
func (t *IteratorTesting) Prev() {
	t.init()
	t.setAct(IterPrev)

	ok := t.Iter.Prev()
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	if t.Pos > 0 {
		t.Pos--
		gomega.Expect(ok).Should(gomega.BeTrue(), t.Text())
		t.TestKV()
	} else {
		t.Pos = -1
		gomega.Expect(ok).ShouldNot(gomega.BeTrue(), t.Text())
	}
	t.post()
}

// Seek seeks to the i-th key and checks the entry.
func (t *IteratorTesting) Seek(i int) {
	t.init()
	t.setAct(IterSeek)

	key, _ := t.Index(i)
	oldKey, _ := t.IndexOrNil(t.Pos)

	ok := t.Iter.Seek(key)
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	gomega.Expect(ok).Should(gomega.BeTrue(), fmt.Sprintf("Seek from key %q to %q, to pos %d, %s", oldKey, key, i, t.text()))

	t.Pos = i
	t.TestKV()
	t.post()
}

// SeekInexact seeks to a key just before the i-th key and checks the entry.
func (t *IteratorTesting) SeekInexact(i int) {
	t.init()
	t.setAct(IterSeek)
	var key0 []byte
	key1, _ := t.Index(i)
	if i > 0 {
		key0, _ = t.Index(i - 1)
	}
	key := BytesSeparator(key0, key1)
	oldKey, _ := t.IndexOrNil(t.Pos)

	ok := t.Iter.Seek(key)
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	gomega.Expect(ok).Should(gomega.BeTrue(), fmt.Sprintf("Seek from key %q to %q (%q), to pos %d, %s", oldKey, key, key1, i, t.text()))

	t.Pos = i
	t.TestKV()
	t.post()
}

// SeekKey seeks to key and checks the entry.
func (t *IteratorTesting) SeekKey(key []byte) {
	t.init()
	t.setAct(IterSeek)
	oldKey, _ := t.IndexOrNil(t.Pos)
	i := t.Search(key)

	ok := t.Iter.Seek(key)
	gomega.Expect(t.Iter.Error()).ShouldNot(gomega.HaveOccurred())
	if i < t.Len() {
		key_, _ := t.Index(i)
		gomega.Expect(ok).Should(gomega.BeTrue(), fmt.Sprintf("Seek from key %q to %q (%q), to pos %d, %s", oldKey, key, key_, i, t.text()))
		t.Pos = i
		t.TestKV()
	} else {
		gomega.Expect(ok).ShouldNot(gomega.BeTrue(), fmt.Sprintf("Seek from key %q to %q, %s", oldKey, key, t.text()))
	}

	t.Pos = i
	t.post()
}

// SOI moves before the first entry and checks the iterator is there.
func (t *IteratorTesting) SOI() {
	t.init()
	t.setAct(IterSOI)
	gomega.Expect(t.Pos).Should(gomega.BeNumerically("<=", 0), t.Text())
	for i := 0; i < 3; i++ {
		t.Prev()
	}
	t.post()
}

// EOI moves after the last entry and checks the iterator is there.
func (t *IteratorTesting) EOI() {
	t.init()
	t.setAct(IterEOI)
	gomega.Expect(t.Pos).Should(gomega.BeNumerically(">=", t.Len()-1), t.Text())
	for i := 0; i < 3; i++ {
		t.Next()
	}
	t.post()
}

// WalkPrev calls fn while moving backward through every entry.
func (t *IteratorTesting) WalkPrev(fn func(t *IteratorTesting)) {
	t.init()
	for old := t.Pos; t.Pos > 0; old = t.Pos {
		fn(t)
		gomega.Expect(t.Pos).Should(gomega.BeNumerically("<", old), t.Text())
	}
}

// WalkNext calls fn while moving forward through every entry.
func (t *IteratorTesting) WalkNext(fn func(t *IteratorTesting)) {
	t.init()
	for old := t.Pos; t.Pos < t.Len()-1; old = t.Pos {
		fn(t)
		gomega.Expect(t.Pos).Should(gomega.BeNumerically(">", old), t.Text())
	}
}

// PrevAll moves backward through every entry, checking each.
func (t *IteratorTesting) PrevAll() {
	t.WalkPrev(func(t *IteratorTesting) {
		t.Prev()
	})
}

// NextAll moves forward through every entry, checking each.
func (t *IteratorTesting) NextAll() {
	t.WalkNext(func(t *IteratorTesting) {
		t.Next()
	})
}

// DoIteratorTesting runs random movements against t and checks each step.
func DoIteratorTesting(t *IteratorTesting) {
	if t.Rand == nil {
		t.Rand = NewRand()
	}
	t.SOI()
	t.NextAll()
	t.First()
	t.SOI()
	t.NextAll()
	t.EOI()
	t.PrevAll()
	t.Last()
	t.EOI()
	t.PrevAll()
	t.SOI()

	t.NextAll()
	t.PrevAll()
	t.NextAll()
	t.Last()
	t.PrevAll()
	t.First()
	t.NextAll()
	t.EOI()

	ShuffledIndex(t.Rand, t.Len(), 1, func(i int) {
		t.Seek(i)
	})

	ShuffledIndex(t.Rand, t.Len(), 1, func(i int) {
		t.SeekInexact(i)
	})

	ShuffledIndex(t.Rand, t.Len(), 1, func(i int) {
		t.Seek(i)
		if i%2 != 0 {
			t.PrevAll()
			t.SOI()
		} else {
			t.NextAll()
			t.EOI()
		}
	})

	for _, key := range []string{"", "foo", "bar", "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"} {
		t.SeekKey([]byte(key))
	}
}
