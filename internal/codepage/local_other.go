//go:build !windows

package codepage

// LocalACP returns 0 on platforms that have no Windows code page —
// the value Decoder treats as "no decoder".
func LocalACP() uint32 {
	return 0
}
