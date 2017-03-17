package lba

import (
	"errors"
	"fmt"
	"io"
)

const (
	//NumberOfRecordsPerLBAShard is the fixed length of the LBAShards
	NumberOfRecordsPerLBAShard = 128
	// BytesPerShard defines how many bytes each shards requires
	BytesPerShard = NumberOfRecordsPerLBAShard * HashSize
)

var (
	errNilShardWrite = errors.New("shard is nil, and cannot be written")
)

func newShard() *shard {
	return new(shard)
}

func shardFromBytes(bytes []byte) (shard *shard, err error) {
	if len(bytes) < BytesPerShard {
		err = fmt.Errorf("raw shard is too small, expected %d bytes", BytesPerShard)
		return
	}

	shard = newShard()
	for i := 0; i < NumberOfRecordsPerLBAShard; i++ {
		h := NewHash()
		copy(h, bytes[i*HashSize:])
		if !h.Equals(nilHash) {
			shard.hashes[i] = h
		}
	}

	return
}

type shard struct {
	hashes [NumberOfRecordsPerLBAShard]Hash
	dirty  bool
}

func (s *shard) Dirty() bool {
	return s.dirty
}

func (s *shard) UnsetDirty() {
	s.dirty = false
}

func (s *shard) Set(hashIndex int64, hash Hash) {
	s.hashes[hashIndex] = hash
	s.dirty = true
}

func (s *shard) Get(hashIndex int64) Hash {
	return s.hashes[hashIndex]
}

func (s *shard) Write(w io.Writer) (err error) {
	var index int

	// keep going until we encounter a non-nilHash
	for index = 0; index < NumberOfRecordsPerLBAShard; index++ {
		if s.hashes[index] != nil {
			break
		}
	}

	// if all were nilHashes, we simply return it as an error
	if index == NumberOfRecordsPerLBAShard {
		err = errNilShardWrite
		return
	}

	// we have non-nil hashes, so let's write all
	// nil hashes that came before the first non-nil hash
	for i := 0; i < index; i++ {
		if _, err = w.Write(nilHash); err != nil {
			return
		}
	}

	// write all other hashes
	var h Hash
	for ; index < NumberOfRecordsPerLBAShard; index++ {
		h = s.hashes[index]
		if h == nil {
			h = nilHash
		}
		if _, err = w.Write(h[:]); err != nil {
			return
		}
	}

	return
}
