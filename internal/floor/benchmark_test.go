package floor

import "testing"

func BenchmarkBuildFrame(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildFrame(1, 8, 64, func(_, channel, position int) RGB {
			return RGB{R: byte(position), G: byte(channel), B: 7}
		})
	}
}
