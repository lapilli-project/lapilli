package rules

import (
	"encoding/binary"
	"math/bits"
)

// xxhash64 is XXH64 with seed 0, as Prometheus hashes a label set with
// (github.com/cespare/xxhash/v2), written out so that Lapilli needn't take
// the module for the one place it reads a group's place.
func xxhash64(b []byte) uint64 {
	var (
		p1 uint64 = 11400714785074694791
		p2 uint64 = 14029467366897019727
		p3 uint64 = 1609587929392839161
		p4 uint64 = 9650029242287828579
		p5 uint64 = 2870177450012600261
	)
	round := func(acc, in uint64) uint64 { return bits.RotateLeft64(acc+in*p2, 31) * p1 }
	merge := func(h, v uint64) uint64 { return (h^round(0, v))*p1 + p4 }
	n := len(b)
	var h uint64
	if n >= 32 {
		v1, v2, v3, v4 := p1+p2, p2, uint64(0), -p1
		for ; len(b) >= 32; b = b[32:] {
			v1 = round(v1, binary.LittleEndian.Uint64(b[0:]))
			v2 = round(v2, binary.LittleEndian.Uint64(b[8:]))
			v3 = round(v3, binary.LittleEndian.Uint64(b[16:]))
			v4 = round(v4, binary.LittleEndian.Uint64(b[24:]))
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = merge(merge(merge(merge(h, v1), v2), v3), v4)
	} else {
		h = p5
	}
	h += uint64(n)
	for ; len(b) >= 8; b = b[8:] {
		h ^= round(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*p1 + p4
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * p1
		h = bits.RotateLeft64(h, 23)*p2 + p3
		b = b[4:]
	}
	for ; len(b) > 0; b = b[1:] {
		h ^= uint64(b[0]) * p5
		h = bits.RotateLeft64(h, 11) * p1
	}
	h ^= h >> 33
	h *= p2
	h ^= h >> 29
	h *= p3
	h ^= h >> 32
	return h
}
