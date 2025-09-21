package util

import "unicode/utf8"

// ByteToRuneIndexMap returns mapping from byte offset -> rune index.
func ByteToRuneIndexMap(s string) []int {
	mapIdx := make([]int, 0, len(s)+1)
	rpos := 0
	for i := 0; i < len(s); {
		mapIdx = append(mapIdx, rpos)
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		rpos++
	}
	mapIdx = append(mapIdx, rpos)
	return mapIdx
}

func ByteIndexToRuneIndex(mapIdx []int, byteIdx int) int {
	if byteIdx < 0 {
		return 0
	}
	if byteIdx >= len(mapIdx) {
		return mapIdx[len(mapIdx)-1]
	}
	return mapIdx[byteIdx]
}
