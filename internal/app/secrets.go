package app

import (
	"errors"
	"io"
	"os"
	"strings"
)

func ReadPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("secret must be a private regular file (mode 0600), not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open secret file")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.New("secret file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return nil, errors.New("secret file is unreadable or too large")
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil, errors.New("secret file is empty")
	}
	return b, nil
}
