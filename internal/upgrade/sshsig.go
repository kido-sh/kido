package upgrade

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const Namespace = "release@kido.sh"

var errMalformed = errors.New("malformed SSH signature")

func parsePublicKey(line string) (ed25519.PublicKey, error) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return nil, errors.New("the release key is not an ssh-ed25519 public key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, fmt.Errorf("the release key: %w", err)
	}
	return parseKeyBlob(blob)
}

func parseKeyBlob(blob []byte) (ed25519.PublicKey, error) {
	r := wire{b: blob}
	typ, key := r.string(), r.string()
	if r.err != nil || len(r.b) != 0 || string(typ) != "ssh-ed25519" || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("not an ssh-ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

type wire struct {
	b   []byte
	err error
}

func (w *wire) string() []byte {
	if w.err != nil {
		return nil
	}
	if len(w.b) < 4 || uint64(binary.BigEndian.Uint32(w.b)) > uint64(len(w.b)-4) {
		w.err = errMalformed
		return nil
	}
	n := binary.BigEndian.Uint32(w.b)
	s := w.b[4 : 4+n]
	w.b = w.b[4+n:]
	return s
}

// The format is OpenSSH's PROTOCOL.sshsig.
func verifySSHSig(key ed25519.PublicKey, msg, armored []byte) error {
	const begin, end = "-----BEGIN SSH SIGNATURE-----", "-----END SSH SIGNATURE-----"
	s := strings.TrimSpace(string(armored))
	if !strings.HasPrefix(s, begin) || !strings.HasSuffix(s, end) {
		return errMalformed
	}
	blob, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s[len(begin):len(s)-len(end)]), ""))
	if err != nil || !bytes.HasPrefix(blob, []byte("SSHSIG")) || len(blob) < 10 {
		return errMalformed
	}
	if binary.BigEndian.Uint32(blob[6:]) != 1 {
		return errors.New("unsupported SSH signature version")
	}
	r := wire{b: blob[10:]}
	pub, ns, reserved, hashAlg, sig := r.string(), r.string(), r.string(), r.string(), r.string()
	if r.err != nil || len(r.b) != 0 {
		return errMalformed
	}
	if signer, err := parseKeyBlob(pub); err != nil || !signer.Equal(key) {
		return errors.New("signed by another key")
	}
	if string(ns) != Namespace {
		return fmt.Errorf("signature namespace is %q, not %q", ns, Namespace)
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(msg)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(msg)
		digest = h[:]
	default:
		return fmt.Errorf("unsupported signature hash %q", hashAlg)
	}
	sr := wire{b: sig}
	typ, raw := sr.string(), sr.string()
	if sr.err != nil || len(sr.b) != 0 || string(typ) != "ssh-ed25519" || len(raw) != ed25519.SignatureSize {
		return errMalformed
	}
	signed := []byte("SSHSIG")
	for _, f := range [][]byte{ns, reserved, hashAlg, digest} {
		signed = binary.BigEndian.AppendUint32(signed, uint32(len(f)))
		signed = append(signed, f...)
	}
	if !ed25519.Verify(key, signed, raw) {
		return errors.New("bad signature")
	}
	return nil
}
