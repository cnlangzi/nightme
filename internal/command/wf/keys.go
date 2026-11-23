package wf

// The Data keys this package's outcomes carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
const (
	// KeyAddress is the workflow address as it was typed.
	KeyAddress = "address"
	// KeyPath is the file the workflow was installed to.
	KeyPath = "path"
	// KeySHA256 is the digest of the installed file.
	KeySHA256 = "sha256"
	// KeySource is the tier that served the file.
	KeySource = "source"
)
