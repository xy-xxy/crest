// Package auth verifies credentials against htpasswd files for auth_basic.
// Supported hash formats: bcrypt ($2y$...), Apache MD5 ($apr1$...), SHA1
// ({SHA}...), and plain text.
package auth

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type File struct {
	users map[string]string
}

func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &File{users: map[string]string{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f.users[user] = hash
	}
	return f, nil
}

func (f *File) Verify(user, pass string) bool {
	hash, ok := f.users[user]
	if !ok {
		return false
	}
	switch {
	case strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$") || strings.HasPrefix(hash, "$2y$"):
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
	case strings.HasPrefix(hash, "$apr1$"):
		rest := hash[len("$apr1$"):]
		salt, _, ok := strings.Cut(rest, "$")
		if !ok {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(hash), []byte(aprMD5(pass, salt))) == 1
	case strings.HasPrefix(hash, "{SHA}"):
		sum := sha1.Sum([]byte(pass))
		want := "{SHA}" + base64.StdEncoding.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(hash), []byte(want)) == 1
	default:
		return subtle.ConstantTimeCompare([]byte(hash), []byte(pass)) == 1
	}
}

// aprMD5 implements Apache's MD5-crypt ($apr1$), htpasswd -m's default.
func aprMD5(pass, salt string) string {
	h := md5.New()
	h.Write([]byte(pass))
	h.Write([]byte("$apr1$"))
	h.Write([]byte(salt))

	alt := md5.Sum([]byte(pass + salt + pass))
	for i := len(pass); i > 0; i -= 16 {
		if i > 16 {
			h.Write(alt[:])
		} else {
			h.Write(alt[:i])
		}
	}
	for i := len(pass); i > 0; i >>= 1 {
		if i&1 == 1 {
			h.Write([]byte{0})
		} else {
			h.Write([]byte(pass[:1]))
		}
	}
	final := h.Sum(nil)

	for i := 0; i < 1000; i++ {
		h2 := md5.New()
		if i&1 == 1 {
			h2.Write([]byte(pass))
		} else {
			h2.Write(final)
		}
		if i%3 != 0 {
			h2.Write([]byte(salt))
		}
		if i%7 != 0 {
			h2.Write([]byte(pass))
		}
		if i&1 == 1 {
			h2.Write(final)
		} else {
			h2.Write([]byte(pass))
		}
		final = h2.Sum(nil)
	}

	const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	enc := func(a, b, c byte, n int, sb *strings.Builder) {
		v := uint(a)<<16 | uint(b)<<8 | uint(c)
		for ; n > 0; n-- {
			sb.WriteByte(itoa64[v&0x3f])
			v >>= 6
		}
	}
	var sb strings.Builder
	enc(final[0], final[6], final[12], 4, &sb)
	enc(final[1], final[7], final[13], 4, &sb)
	enc(final[2], final[8], final[14], 4, &sb)
	enc(final[3], final[9], final[15], 4, &sb)
	enc(final[4], final[10], final[5], 4, &sb)
	enc(0, 0, final[11], 2, &sb)
	return "$apr1$" + salt + "$" + sb.String()
}
