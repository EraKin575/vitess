/*
Copyright 2019 The Vitess Authors.

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

package vindexes

import (
	"bytes"
	"context"
	"fmt"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/key"
)

var (
	_ SingleColumn    = (*Binary)(nil)
	_ Reversible      = (*Binary)(nil)
	_ Hashing         = (*Binary)(nil)
	_ ParamValidating = (*Binary)(nil)
	_ Sequential      = (*Binary)(nil)
)

// Binary is a vindex that converts binary bits to a keyspace id.
type Binary struct {
	name          string
	unknownParams []string
}

// newBinary creates a new Binary.
func newBinary(name string, params map[string]string) (Vindex, error) {
	return &Binary{
		name:          name,
		unknownParams: FindUnknownParams(params, nil),
	}, nil
}

// String returns the name of the vindex.
func (vind *Binary) String() string {
	return vind.name
}

// Cost returns the cost as 1.
func (vind *Binary) Cost() int {
	return 0
}

// IsUnique returns true since the Vindex is unique.
func (vind *Binary) IsUnique() bool {
	return true
}

// NeedsVCursor satisfies the Vindex interface.
func (vind *Binary) NeedsVCursor() bool {
	return false
}

// Verify returns true if ids maps to ksids.
func (vind *Binary) Verify(ctx context.Context, vcursor VCursor, ids []sqltypes.Value, ksids [][]byte) ([]bool, error) {
	out := make([]bool, 0, len(ids))
	for i, id := range ids {
		idBytes, err := vind.Hash(id)
		if err != nil {
			return out, err
		}
		out = append(out, bytes.Equal(idBytes, ksids[i]))
	}
	return out, nil
}

// Map can map ids to key.ShardDestination objects.
func (vind *Binary) Map(ctx context.Context, vcursor VCursor, ids []sqltypes.Value) ([]key.ShardDestination, error) {
	out := make([]key.ShardDestination, 0, len(ids))
	for _, id := range ids {
		idBytes, err := vind.Hash(id)
		if err != nil {
			return out, err
		}
		out = append(out, key.DestinationKeyspaceID(idBytes))
	}
	return out, nil
}

func (vind *Binary) Hash(id sqltypes.Value) ([]byte, error) {
	return id.ToBytes()
}

// ReverseMap returns the associated ids for the ksids.
func (*Binary) ReverseMap(_ VCursor, ksids [][]byte) ([]sqltypes.Value, error) {
	var reverseIds = make([]sqltypes.Value, len(ksids))
	for rownum, keyspaceID := range ksids {
		if keyspaceID == nil {
			return nil, fmt.Errorf("Binary.ReverseMap: keyspaceId is nil")
		}
		reverseIds[rownum] = sqltypes.MakeTrusted(sqltypes.VarBinary, keyspaceID)
	}
	return reverseIds, nil
}

// RangeMap can map ids to key.ShardDestination objects.
func (vind *Binary) RangeMap(ctx context.Context, vcursor VCursor, startId sqltypes.Value, endId sqltypes.Value) ([]key.ShardDestination, error) {
	startKsId, err := vind.Hash(startId)
	if err != nil {
		return nil, err
	}
	endKsId, err := vind.Hash(endId)
	if err != nil {
		return nil, err
	}
	out := []key.ShardDestination{&key.DestinationKeyRange{KeyRange: key.NewKeyRange(startKsId, endKsId)}}
	return out, nil
}

// UnknownParams implements the ParamValidating interface.
func (vind *Binary) UnknownParams() []string {
	return vind.unknownParams
}

func init() {
	Register("binary", newBinary)
}
