//go:build !unix

package smem

// fileIdentity is unsupported here; copies are not detected.
func fileIdentity(string) string { return "" }
