package selfupdate

// ReleaseKeys are the minisign public keys whose signature on checksums.txt is
// accepted. Keep keys/release_sign.pub in sync; to rotate, add the new key here
// and release once before signing with it.
var ReleaseKeys = []string{
	// keys/release_sign.pub, key id 2BAA15AB209C501C
	"RWQcUJwgqxWqKxfkPfi/WyRuUeKTEDaS9Q/uQJIb7ZQIuqroLtb8f7OH",
}
