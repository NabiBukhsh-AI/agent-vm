package gitguard

// linked reports whether the regular file p has other hard links. Not
// checked on Windows.
func linked(p string) bool { return false }
