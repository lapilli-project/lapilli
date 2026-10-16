package local

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Key file formats.
const (
	FormatOpenSSH = "openssh" // -----BEGIN OPENSSH PRIVATE KEY-----
	FormatPEM     = "pem"     // RSA / EC / DSA / PKCS#8 PEM
	FormatPuTTY   = "putty"   // .ppk, which OpenSSH cannot read
	FormatPublic  = "public"  // a one-line OpenSSH public key
	FormatRFC4716 = "rfc4716" // ---- BEGIN SSH2 PUBLIC KEY ----
	FormatUnknown = "unknown"
)

// KeyFile is what lapilli can tell about a key file without decrypting it.
type KeyFile struct {
	Path        string      `json:"path"`
	Exists      bool        `json:"exists"`
	Mode        fs.FileMode `json:"-"`
	ModeString  string      `json:"mode,omitempty"`
	OwnedByUser bool        `json:"owned_by_user"`
	Format      string      `json:"format,omitempty"`
	Encrypted   bool        `json:"encrypted"`
	Type        string      `json:"type,omitempty"` // ED25519, RSA, ECDSA, ED25519-SK, ECDSA-SK, DSA
	Bits        int         `json:"bits,omitempty"`
	Fingerprint string      `json:"fingerprint,omitempty"`
	PubExists   bool        `json:"pub_exists"`
	PubMismatch bool        `json:"pub_mismatch,omitempty"` // the .pub beside it is a different key
	Err         string      `json:"error,omitempty"`
}

// Private reports whether the file holds a private key ssh could be pointed at.
func (k KeyFile) Private() bool {
	return k.Format == FormatOpenSSH || k.Format == FormatPEM || k.Format == FormatPuTTY
}

// TooOpen reports whether ssh would refuse the private key for its permissions.
func (k KeyFile) TooOpen() bool {
	return k.Exists && k.OwnedByUser && k.Private() && k.Format != FormatPuTTY && k.Mode.Perm()&0o077 != 0
}

const maxKeyFile = 64 * 1024

// InspectKey reads one key file. It never decrypts anything: for OpenSSH-format
// keys the public half is stored in the clear, and for PEM keys it is derived
// only when the file is not encrypted.
func InspectKey(path string) KeyFile {
	k := KeyFile{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			k.Err = err.Error()
		}
		return k
	}
	k.Exists = true
	k.Mode = fi.Mode()
	k.ModeString = "0" + strconvOctal(uint32(fi.Mode().Perm()))
	k.OwnedByUser = ownedByCurrentUser(fi)
	if !fi.Mode().IsRegular() {
		k.Err = "not a regular file"
		return k
	}
	data, err := readCapped(path)
	if err != nil {
		k.Err = err.Error()
		return k
	}
	k.Format, k.Type, k.Bits, k.Fingerprint, k.Encrypted = identify(data)

	if k.Format != FormatPublic {
		if pub, err := readCapped(path + ".pub"); err == nil {
			k.PubExists = true
			if f, _, _, fp, _ := identify(pub); f == FormatPublic && fp != "" {
				if k.Fingerprint == "" {
					_, k.Type, k.Bits, k.Fingerprint, _ = identify(pub)
				} else if fp != k.Fingerprint {
					k.PubMismatch = true
				}
			}
		}
	}
	return k
}

// ScanKeys lists the private key files in dir (normally ~/.ssh), sorted by name.
func ScanKeys(dir string) []KeyFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []KeyFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".pub") || strings.HasPrefix(name, ".") || skipName[name] ||
			strings.HasPrefix(name, "known_hosts") || strings.HasPrefix(name, "authorized_keys") {
			continue
		}
		if info, err := e.Info(); err != nil || !info.Mode().IsRegular() || info.Size() > maxKeyFile {
			continue
		}
		k := InspectKey(filepath.Join(dir, name))
		if k.Private() {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

var skipName = map[string]bool{"config": true, "environment": true, "rc": true, "allowed_signers": true}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxKeyFile))
}

// identify works out a key file's format, type, size and SHA256 fingerprint.
func identify(data []byte) (format, typ string, bits int, fp string, encrypted bool) {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(trimmed, []byte("PuTTY-User-Key-File-")):
		line, _, _ := strings.Cut(string(trimmed), "\n")
		if _, alg, ok := strings.Cut(line, ": "); ok {
			typ = typeOf(strings.TrimSpace(alg))
		}
		encrypted = !strings.Contains(string(trimmed), "Encryption: none")
		return FormatPuTTY, typ, 0, "", encrypted
	case bytes.HasPrefix(trimmed, []byte("---- BEGIN SSH2 PUBLIC KEY ----")):
		return FormatRFC4716, "", 0, "", false
	case looksLikePublicKey(trimmed):
		fields := strings.Fields(string(trimmed))
		blob, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil {
			return FormatUnknown, "", 0, "", false
		}
		typ, bits = describeBlob(blob)
		return FormatPublic, typ, bits, Fingerprint(blob), false
	}
	block, _ := pem.Decode(trimmed)
	if block == nil {
		return FormatUnknown, "", 0, "", false
	}
	switch block.Type {
	case "OPENSSH PRIVATE KEY":
		blob, cipher, ok := parseOpenSSHPrivate(block.Bytes)
		if !ok {
			return FormatOpenSSH, "", 0, "", false
		}
		typ, bits = describeBlob(blob)
		return FormatOpenSSH, typ, bits, Fingerprint(blob), cipher != "none"
	case "ENCRYPTED PRIVATE KEY":
		return FormatPEM, "", 0, "", true
	case "DSA PRIVATE KEY":
		return FormatPEM, "DSA", 0, "", block.Headers["Proc-Type"] == "4,ENCRYPTED"
	}
	if block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		switch block.Type {
		case "RSA PRIVATE KEY":
			typ = "RSA"
		case "EC PRIVATE KEY":
			typ = "ECDSA"
		}
		return FormatPEM, typ, 0, "", true
	}
	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return FormatUnknown, "", 0, "", false
	}
	if err != nil {
		return FormatPEM, "", 0, "", false
	}
	blob := publicBlob(key)
	if blob == nil {
		return FormatPEM, "", 0, "", false
	}
	typ, bits = describeBlob(blob)
	return FormatPEM, typ, bits, Fingerprint(blob), false
}

func looksLikePublicKey(b []byte) bool {
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return false
	}
	t := fields[0]
	return strings.HasPrefix(t, "ssh-") || strings.HasPrefix(t, "ecdsa-sha2-") || strings.HasPrefix(t, "sk-")
}

// Fingerprint is ssh-keygen's SHA256 fingerprint of a public key blob.
func Fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// parseOpenSSHPrivate returns the public key blob and cipher name of an
// openssh-key-v1 file. Both sit in the unencrypted header.
func parseOpenSSHPrivate(b []byte) (blob []byte, cipher string, ok bool) {
	const magic = "openssh-key-v1\x00"
	if !bytes.HasPrefix(b, []byte(magic)) {
		return nil, "", false
	}
	r := b[len(magic):]
	c, r, ok := readString(r)
	if !ok {
		return nil, "", false
	}
	if _, r, ok = readString(r); !ok { // kdfname
		return nil, "", false
	}
	if _, r, ok = readString(r); !ok { // kdfoptions
		return nil, "", false
	}
	if len(r) < 4 || binary.BigEndian.Uint32(r) < 1 {
		return nil, "", false
	}
	blob, _, ok = readString(r[4:])
	return blob, string(c), ok
}

func readString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// describeBlob names a public key blob the way ssh's log does, with its size.
func describeBlob(blob []byte) (typ string, bits int) {
	name, rest, ok := readString(blob)
	if !ok {
		return "", 0
	}
	typ = typeOf(string(name))
	switch typ {
	case "RSA":
		if _, rest, ok = readString(rest); ok { // e
			if n, _, ok := readString(rest); ok {
				bits = new(big.Int).SetBytes(n).BitLen()
			}
		}
	case "ED25519", "ED25519-SK":
		bits = 256
	case "ECDSA", "ECDSA-SK":
		switch {
		case strings.Contains(string(name), "nistp256"):
			bits = 256
		case strings.Contains(string(name), "nistp384"):
			bits = 384
		case strings.Contains(string(name), "nistp521"):
			bits = 521
		}
	}
	return typ, bits
}

func typeOf(alg string) string {
	switch {
	case alg == "ssh-ed25519":
		return "ED25519"
	case alg == "ssh-rsa":
		return "RSA"
	case alg == "ssh-dss":
		return "DSA"
	case strings.HasPrefix(alg, "ecdsa-sha2-"):
		return "ECDSA"
	case strings.HasPrefix(alg, "sk-ssh-ed25519"):
		return "ED25519-SK"
	case strings.HasPrefix(alg, "sk-ecdsa-sha2-"):
		return "ECDSA-SK"
	case strings.HasPrefix(alg, "ssh-ed25519-cert"):
		return "ED25519-CERT"
	case strings.HasPrefix(alg, "ssh-rsa-cert"):
		return "RSA-CERT"
	}
	return strings.ToUpper(alg)
}

// publicBlob builds the SSH wire encoding of a parsed PEM private key's public half.
func publicBlob(key any) []byte {
	var b bytes.Buffer
	switch k := key.(type) {
	case *rsa.PrivateKey:
		writeString(&b, []byte("ssh-rsa"))
		writeMPInt(&b, big.NewInt(int64(k.E)))
		writeMPInt(&b, k.N)
	case *ecdsa.PrivateKey:
		ec, err := k.PublicKey.ECDH()
		if err != nil {
			return nil
		}
		curve := map[int]string{256: "nistp256", 384: "nistp384", 521: "nistp521"}[k.Curve.Params().BitSize]
		if curve == "" {
			return nil
		}
		writeString(&b, []byte("ecdsa-sha2-"+curve))
		writeString(&b, []byte(curve))
		writeString(&b, ec.Bytes())
	case ed25519.PrivateKey:
		writeString(&b, []byte("ssh-ed25519"))
		writeString(&b, k.Public().(ed25519.PublicKey))
	default:
		return nil
	}
	return b.Bytes()
}

func writeString(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}

func writeMPInt(b *bytes.Buffer, x *big.Int) {
	v := x.Bytes()
	if len(v) > 0 && v[0]&0x80 != 0 {
		v = append([]byte{0}, v...)
	}
	writeString(b, v)
}

func strconvOctal(v uint32) string {
	const digits = "01234567"
	out := []byte{digits[(v>>6)&7], digits[(v>>3)&7], digits[v&7]}
	return string(out)
}
