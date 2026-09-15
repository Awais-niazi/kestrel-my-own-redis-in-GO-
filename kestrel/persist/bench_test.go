package persist

import (
	"path/filepath"
	"testing"
)

func benchLog(b *testing.B, policy Fsync) *segment {
	b.Helper()
	l, err := createSegment(segmentOptions{Path: filepath.Join(b.TempDir(), "bench.log"), Fsync: policy})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { l.Close() })
	return l
}

func BenchmarkAppend(b *testing.B) {
	for _, policy := range []Fsync{FsyncNo, FsyncEverySec, FsyncAlways} {
		b.Run(policy.String(), func(b *testing.B) {
			l := benchLog(b, policy)
			args := cmd("SET", "somekey", "somevalue")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := l.Append(0, args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkAppendParallel(b *testing.B) {
	l := benchLog(b, FsyncEverySec)
	args := cmd("SET", "somekey", "somevalue")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Append(0, args); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkEncodeRecord(b *testing.B) {
	args := cmd("SET", "somekey", "somevalue")
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = encodeRecord(buf[:0], 0, KindEffect, args)
	}
}
