package text

import (
	"crypto/md5"
	"encoding/binary"
	"strings"
)

func IntHash(text string) (int, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	sum := md5.Sum([]byte(text))
	seed := binary.BigEndian.Uint32(sum[:])
	return int(seed), nil
}
