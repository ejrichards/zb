// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package lua

import (
	"errors"
	"iter"
	"math"
	"slices"
	"sort"

	"zb.256lights.llc/pkg/internal/luacode"
)

type table struct {
	id      uint64
	entries []tableEntry
	meta    *table
	frozen  bool
}

func newTable(capacity int) *table {
	tab := &table{id: nextID()}
	if capacity > 0 {
		tab.entries = make([]tableEntry, 0, capacity)
	}
	return tab
}

func (tab *table) valueType() Type {
	return TypeTable
}

func (tab *table) valueID() uint64 {
	return tab.id
}

func (tab *table) references(*State) iter.Seq[referenceValue] {
	return func(yield func(referenceValue) bool) {
		if tab.meta != nil {
			if !yield(tab.meta) {
				return
			}
		}
		for _, ent := range tab.entries {
			if k, ok := ent.key.(referenceValue); ok && !yield(k) {
				return
			}
			if v, ok := ent.value.(referenceValue); ok && !yield(v) {
				return
			}
		}
	}
}

// len returns a [border in the table].
// This is equivalent to the Lua length ("#") operator.
//
// [border in the table]: https://lua.org/manual/5.4/manual.html#3.4.7
func (tab *table) len() integerValue {
	if tab == nil {
		return 0
	}
	start, ok := findEntry(tab.entries, integerValue(1))
	if !ok {
		return 0
	}

	// Find the last entry with a numeric key in the possible range.
	// For example, if len(tab.entries) - start == 3,
	// then we can ignore any values greater than 3
	// because there necessarily must be a border before any of those values.
	maxKey := len(tab.entries) - start
	searchSpace := tab.entries[start+1:] // Can skip 1.
	n := sort.Search(len(searchSpace), func(i int) bool {
		switch k := searchSpace[i].key.(type) {
		case integerValue:
			return k > integerValue(maxKey)
		case floatValue:
			return k > floatValue(maxKey)
		default:
			return true
		}
	})
	searchSpace = searchSpace[:n]
	// Maximum key cannot be larger than the number of elements
	// (plus one, because we excluded the 1 entry).
	maxKey = n + 1

	// Instead of searching over slice indices,
	// we binary search over the key space to find the first i
	// for which table[i + 1] == nil.
	i := sort.Search(maxKey, func(i int) bool {
		_, found := findEntry(searchSpace, integerValue(i)+2)
		return !found
	})
	return integerValue(i) + 1
}

func (tab *table) get(key value) value {
	if tab == nil || key == nil {
		return nil
	}
	i, found := findEntry(tab.entries, key)
	if !found {
		return nil
	}
	return tab.entries[i].value
}

// set sets the value for the given key.
// If the key cannot be used as a table key, then set returns an error.
// If the table is frozen, then set returns [errFrozenTable].
// set panics if tab is nil.
func (tab *table) set(key, value value) error {
	if tab.frozen {
		return errFrozenTable
	}

	switch k := key.(type) {
	case nil:
		return errors.New("table index is nil")
	case floatValue:
		if math.IsNaN(float64(k)) {
			return errors.New("table index is NaN")
		}
		if i, ok := luacode.FloatToInteger(float64(k), luacode.OnlyIntegral); ok {
			key = integerValue(i)
		}
	}

	i, found := findEntry(tab.entries, key)
	switch {
	case found && value != nil:
		tab.entries[i].value = value
	case found && value == nil:
		tab.entries = slices.Delete(tab.entries, i, i+1)
	case !found && value != nil:
		tab.entries = slices.Insert(tab.entries, i, tableEntry{
			key:   key,
			value: value,
		})
	}
	return nil
}

// setExisting looks up a key in the table
// and changes or removes the value for the key as appropriate.
// If the key was not found in the table, then setExisting returns [errKeyNotFound].
// If the table is frozen, then setExisting returns [errFrozenTable].
// setExisting panics if tab is nil.
func (tab *table) setExisting(k, v value) error {
	if tab.frozen {
		return errFrozenTable
	}
	i, found := findEntry(tab.entries, k)
	if !found {
		return errKeyNotFound
	}
	if v == nil {
		tab.entries = slices.Delete(tab.entries, i, i+1)
	} else {
		tab.entries[i].value = v
	}
	return nil
}

// next returns the next table entry after the given key
// in ascending order (as determined by [compareValues]).
// Passing a nil key returns the first entry in the table.
// If there are no more elements in the table,
// the key of the returned tableEntry is nil.
func (tab *table) next(k value) tableEntry {
	if tab == nil {
		return tableEntry{}
	}
	i := 0
	if k != nil {
		var found bool
		i, found = findEntry(tab.entries, k)
		if found {
			i++
		}
	}
	if i >= len(tab.entries) {
		return tableEntry{}
	}
	return tab.entries[i]
}

type tableEntry struct {
	key, value value
}

func findEntry(entries []tableEntry, key value) (int, bool) {
	return slices.BinarySearchFunc(entries, key, func(e tableEntry, key value) int {
		result, _ := compareValues(e.key, key)
		return result
	})
}

// Errors returned from [*table.set] and [*table.setExisting].
var (
	errKeyNotFound = errors.New("table index not found")
	errFrozenTable = errors.New("attempt to assign to a frozen table")
)
