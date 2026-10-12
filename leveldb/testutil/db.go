// Copyright (c) 2014, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package testutil holds helpers shared by the leveldb tests. Library code
// never imports it.
package testutil

import (
	"fmt"
	"math/rand"

	"github.com/onsi/gomega"

	"github.com/helix-bedrock/world/leveldb/errors"
	"github.com/helix-bedrock/world/leveldb/iterator"
	"github.com/helix-bedrock/world/leveldb/util"
)

// DB is the database interface the DB tests drive.
type DB interface{}

// Put is a DB that writes a key.
type Put interface {
	TestPut(key []byte, value []byte) error
}

// Delete is a DB that deletes a key.
type Delete interface {
	TestDelete(key []byte) error
}

// Find is a DB that finds the first key not less than a key.
type Find interface {
	TestFind(key []byte) (rkey, rvalue []byte, err error)
}

// Get is a DB that reads a key.
type Get interface {
	TestGet(key []byte) (value []byte, err error)
}

// Has is a DB that checks for a key.
type Has interface {
	TestHas(key []byte) (ret bool, err error)
}

// NewIterator is a DB that iterates a key range.
type NewIterator interface {
	TestNewIterator(slice *util.Range) iterator.Iterator
}

// DBAct is a random action DBTesting can perform.
type DBAct int

func (a DBAct) String() string {
	switch a {
	case DBNone:
		return "none"
	case DBPut:
		return "put"
	case DBOverwrite:
		return "overwrite"
	case DBDelete:
		return "delete"
	case DBDeleteNA:
		return "delete_na"
	}
	return "unknown"
}

// DB actions.
const (
	DBNone DBAct = iota
	DBPut
	DBOverwrite
	DBDelete
	DBDeleteNA
)

// DBTesting checks a DB against an in-memory model of its contents.
type DBTesting struct {
	Rand *rand.Rand
	DB   interface {
		Get
		Put
		Delete
	}
	PostFn             func(t *DBTesting)
	Deleted, Present   KeyValue
	Act, LastAct       DBAct
	ActKey, LastActKey []byte
}

func (t *DBTesting) post() {
	if t.PostFn != nil {
		t.PostFn(t)
	}
}

func (t *DBTesting) setAct(act DBAct, key []byte) {
	t.LastAct, t.Act = t.Act, act
	t.LastActKey, t.ActKey = t.ActKey, key
}

func (t *DBTesting) text() string {
	return fmt.Sprintf("last action was <%v> %q, <%v> %q", t.LastAct, t.LastActKey, t.Act, t.ActKey)
}

// Text describes the last action, for failure messages.
func (t *DBTesting) Text() string {
	return "DBTesting " + t.text()
}

// TestPresentKV checks that key holds value.
func (t *DBTesting) TestPresentKV(key, value []byte) {
	rvalue, err := t.DB.TestGet(key)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "Get on key %q, %s", key, t.text())
	gomega.Expect(rvalue).Should(gomega.Equal(value), "Value for key %q, %s", key, t.text())
}

// TestAllPresent checks that every key in the model is present.
func (t *DBTesting) TestAllPresent() {
	t.Present.IterateShuffled(t.Rand, func(i int, key, value []byte) {
		t.TestPresentKV(key, value)
	})
}

// TestDeletedKey checks that key is absent.
func (t *DBTesting) TestDeletedKey(key []byte) {
	_, err := t.DB.TestGet(key)
	gomega.Expect(err).Should(gomega.Equal(errors.ErrNotFound), "Get on deleted key %q, %s", key, t.text())
}

// TestAllDeleted checks that every deleted key is absent.
func (t *DBTesting) TestAllDeleted() {
	t.Deleted.IterateShuffled(t.Rand, func(i int, key, value []byte) {
		t.TestDeletedKey(key)
	})
}

// TestAll checks the whole DB against the model, including iteration.
func (t *DBTesting) TestAll() {
	dn := t.Deleted.Len()
	pn := t.Present.Len()
	ShuffledIndex(t.Rand, dn+pn, 1, func(i int) {
		if i >= dn {
			key, value := t.Present.Index(i - dn)
			t.TestPresentKV(key, value)
		} else {
			t.TestDeletedKey(t.Deleted.KeyAt(i))
		}
	})
}

// Put writes key and value to the DB and the model.
func (t *DBTesting) Put(key, value []byte) {
	if new := t.Present.PutU(key, value); new {
		t.setAct(DBPut, key)
	} else {
		t.setAct(DBOverwrite, key)
	}
	t.Deleted.Delete(key)
	err := t.DB.TestPut(key, value)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), t.Text())
	t.TestPresentKV(key, value)
	t.post()
}

// PutRandom writes a random key that is not yet present.
func (t *DBTesting) PutRandom() bool {
	if t.Deleted.Len() > 0 {
		i := t.Rand.Intn(t.Deleted.Len())
		key, value := t.Deleted.Index(i)
		t.Put(key, value)
		return true
	}
	return false
}

// Delete removes key from the DB and the model.
func (t *DBTesting) Delete(key []byte) {
	if exist, value := t.Present.Delete(key); exist {
		t.setAct(DBDelete, key)
		t.Deleted.PutU(key, value)
	} else {
		t.setAct(DBDeleteNA, key)
	}
	err := t.DB.TestDelete(key)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), t.Text())
	t.TestDeletedKey(key)
	t.post()
}

// DeleteRandom removes a random present key.
func (t *DBTesting) DeleteRandom() bool {
	if t.Present.Len() > 0 {
		i := t.Rand.Intn(t.Present.Len())
		t.Delete(t.Present.KeyAt(i))
		return true
	}
	return false
}

// RandomAct performs round random puts or deletes.
func (t *DBTesting) RandomAct(round int) {
	for i := 0; i < round; i++ {
		if t.Rand.Int()%2 == 0 {
			t.PutRandom()
		} else {
			t.DeleteRandom()
		}
	}
}

// DoDBTesting runs random actions against t and checks the result.
func DoDBTesting(t *DBTesting) {
	if t.Rand == nil {
		t.Rand = NewRand()
	}

	t.DeleteRandom()
	t.PutRandom()
	t.DeleteRandom()
	t.DeleteRandom()
	for i := t.Deleted.Len() / 2; i >= 0; i-- {
		t.PutRandom()
	}
	t.RandomAct((t.Deleted.Len() + t.Present.Len()) * 10)

	// Additional iterator testing
	if db, ok := t.DB.(NewIterator); ok {
		iter := db.TestNewIterator(nil)
		gomega.Expect(iter.Error()).NotTo(gomega.HaveOccurred())

		it := IteratorTesting{
			KeyValue: t.Present,
			Iter:     iter,
		}

		DoIteratorTesting(&it)
		iter.Release()
	}
}
