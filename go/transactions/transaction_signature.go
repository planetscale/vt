/*
Copyright 2024 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package transactions

import (
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
	"strconv"

	"vitess.io/vitess/go/vt/sqlparser"
)

type (
	TxSignature struct {
		Count   int       `json:"count"`
		Queries []TxQuery `json:"qqueries"`
	}

	TxQuery struct {
		Op             string          `json:"op"`
		AffectedTable  string          `json:"affected_table"`
		UpdatedColumns []string        `json:"updated_columns,omitempty"`
		Predicates     []predicateInfo `json:"predicates,omitempty"`
	}

	txSignatureMap struct {
		data map[uint64][]*TxSignature
	}

	predicateInfo struct {
		Table string                           `json:"table"`
		Col   string                           `json:"col"`
		Op    sqlparser.ComparisonExprOperator `json:"op"`
		Val   int                              `json:"val"`
	}
)

func (pi predicateInfo) String() string {
	val := strconv.Itoa(pi.Val)
	if pi.Val == -1 {
		val = "?"
	}
	return fmt.Sprintf("%s.%s %s %s", pi.Table, pi.Col, pi.Op.ToString(), val)
}

func (tx *TxSignature) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Count   int       `json:"count"`
		Queries []TxQuery `json:"query-signatures"`
	}{
		Count:   tx.Count,
		Queries: tx.Queries,
	})
}

func (tx *TxSignature) Hash64() uint64 {
	hasher := fnv.New64a()

	for _, query := range tx.Queries {
		query.addToHash(hasher)
	}

	return hasher.Sum64()
}

func (tx TxQuery) addToHash(hash hash.Hash64) {
	_, _ = hash.Write([]byte(tx.Op))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(tx.AffectedTable))
	_, _ = hash.Write([]byte{0})

	for _, col := range tx.UpdatedColumns {
		_, _ = hash.Write([]byte(col))
		_, _ = hash.Write([]byte{0})
	}

	for _, pred := range tx.Predicates {
		_, _ = hash.Write([]byte(pred.String()))
		_, _ = hash.Write([]byte{0})
	}
}

func (tx TxQuery) Equals(other TxQuery) bool {
	if tx.Op != other.Op {
		return false
	}
	if tx.AffectedTable != other.AffectedTable {
		return false
	}
	if len(tx.UpdatedColumns) != len(other.UpdatedColumns) {
		return false
	}
	for i := range tx.UpdatedColumns {
		if tx.UpdatedColumns[i] != other.UpdatedColumns[i] {
			return false
		}
	}
	if len(tx.Predicates) != len(other.Predicates) {
		return false
	}
	for i := range tx.Predicates {
		if tx.Predicates[i] != other.Predicates[i] {
			return false
		}
	}
	return true
}

func newTxSignatureMap() *txSignatureMap {
	return &txSignatureMap{
		data: make(map[uint64][]*TxSignature),
	}
}

func (m *txSignatureMap) Add(tx *TxSignature) {
	hash := tx.Hash64()

	bucket, exists := m.data[hash]

	// Check if the hash already exists
	if !exists {
		tx.Count = 1
		m.data[hash] = []*TxSignature{tx}
		return
	}

	// Iterate over the bucket to check for exact match
	for _, existingTx := range bucket {
		if tx.Equals(existingTx) {
			existingTx.Count++
			return
		}
	}

	// No exact match found; append to the bucket
	m.data[hash] = append(bucket, tx)
}

func (tx *TxSignature) Equals(other *TxSignature) bool {
	if len(tx.Queries) != len(other.Queries) {
		return false
	}
	for i := range tx.Queries {
		if !tx.Queries[i].Equals(other.Queries[i]) {
			return false
		}
	}

	return true
}

// CleanUp removes values that are only used once and replaces them with -1
func (tx *TxSignature) CleanUp() *TxSignature {
	usedValues := make(map[int]int)

	// First let's count how many times each value is used
	for _, query := range tx.Queries {
		for _, predicate := range query.Predicates {
			usedValues[predicate.Val]++
		}
	}

	// Now we replace values only used once with -1
	newCount := 0
	newValues := make(map[int]int)
	newQueries := make([]TxQuery, 0, len(tx.Queries))
	for _, query := range tx.Queries {
		newPredicates := make([]predicateInfo, 0, len(query.Predicates))
		for _, predicate := range query.Predicates {
			if usedValues[predicate.Val] == 1 {
				predicate.Val = -1
			} else {
				newVal, found := newValues[predicate.Val]
				if !found {
					// Assign a new value to this predicate
					newVal = newCount
					newCount++
					newValues[predicate.Val] = newVal
				}
				predicate.Val = newVal
			}
			newPredicates = append(newPredicates, predicate)
		}
		newQueries = append(newQueries, TxQuery{
			Op:             query.Op,
			AffectedTable:  query.AffectedTable,
			UpdatedColumns: query.UpdatedColumns,
			Predicates:     newPredicates,
		})
	}

	return &TxSignature{
		Queries: newQueries,
		Count:   tx.Count,
	}
}

func (m *txSignatureMap) MarshalJSON() ([]byte, error) {
	// Collect all interesting TxSignatures into a slice
	var signatures []*TxSignature
	for _, bucket := range m.data {
		for _, txSig := range bucket {
			if txSig.Count > 1 {
				signatures = append(signatures, txSig.CleanUp())
			}
		}
	}

	sort.Slice(signatures, func(i, j int) bool {
		return signatures[i].Count > signatures[j].Count
	})

	result := map[string]any{
		"fileType":   "transactions",
		"signatures": signatures,
	}

	return json.Marshal(result)
}
