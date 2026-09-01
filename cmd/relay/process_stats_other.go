//go:build !darwin

package main

func SampleRSSByRoot(rootPIDs []int) map[int]uint64 {
	return map[int]uint64{}
}
