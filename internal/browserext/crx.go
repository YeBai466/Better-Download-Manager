// Package browserext packs the bundled browser extension into a signed CRX3
// file and produces the Omaha update manifest needed for policy force-install.
package browserext

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Bundle holds the artifacts required to force-install the extension.
type Bundle struct {
	ID        string // 32-char Chrome extension id derived from the signing key
	Version   string // extension version from manifest.json
	CRX       []byte // signed CRX3 package
	UpdateXML []byte // Omaha update manifest referencing the CRX
}

// Build packs extFiles into a signed CRX3 using a persistent RSA key stored in
// keyDir (generated on first use so the extension id is stable across runs).
// codebaseURL is the base URL the local server exposes, e.g.
// "http://127.0.0.1:9614/ext"; the CRX is served at <codebaseURL>/bdm.crx.
func Build(extFiles fs.FS, keyDir, codebaseURL string) (*Bundle, error) {
	key, err := loadOrCreateKey(keyDir)
	if err != nil {
		return nil, err
	}

	version, err := readVersion(extFiles)
	if err != nil {
		return nil, err
	}

	zipData, err := zipFS(extFiles)
	if err != nil {
		return nil, err
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	crxID := crxIDFromPubKey(pubDER)
	id := encodeExtensionID(crxID)

	crx, err := packCRX3(key, pubDER, crxID, zipData)
	if err != nil {
		return nil, err
	}

	updateXML := buildUpdateXML(id, version, strings.TrimRight(codebaseURL, "/")+"/bdm.crx")
	return &Bundle{ID: id, Version: version, CRX: crx, UpdateXML: updateXML}, nil
}

func loadOrCreateKey(keyDir string) (*rsa.PrivateKey, error) {
	path := filepath.Join(keyDir, "ext_key.pem")
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func readVersion(extFiles fs.FS) (string, error) {
	data, err := fs.ReadFile(extFiles, "manifest.json")
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version == "" {
		m.Version = "1.0.0"
	}
	return m.Version, nil
}

// zipFS writes every file in extFiles into a zip archive (extension root).
func zipFS(extFiles fs.FS) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(extFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		w, err := zw.Create(path)
		if err != nil {
			return err
		}
		f, err := extFiles.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func crxIDFromPubKey(pubDER []byte) []byte {
	sum := sha256.Sum256(pubDER)
	return sum[:16]
}

// encodeExtensionID maps the 16-byte crx id to the 32-char a-p alphabet Chrome
// uses for extension ids.
func encodeExtensionID(crxID []byte) string {
	var sb strings.Builder
	for _, b := range crxID {
		sb.WriteByte('a' + (b >> 4))
		sb.WriteByte('a' + (b & 0x0f))
	}
	return sb.String()
}

// packCRX3 assembles a CRX3 file: "Cr24" magic, version 3, the CrxFileHeader
// protobuf, then the zip archive. The RSA signature covers the documented
// "CRX3 SignedData\x00" + len + signed_header_data + zip payload.
func packCRX3(key *rsa.PrivateKey, pubDER, crxID, zipData []byte) ([]byte, error) {
	signedHeaderData := protoBytes(1, crxID) // SignedData{ crx_id }

	signingInput := bytes.NewBuffer(nil)
	signingInput.WriteString("CRX3 SignedData\x00")
	_ = binary.Write(signingInput, binary.LittleEndian, uint32(len(signedHeaderData)))
	signingInput.Write(signedHeaderData)
	signingInput.Write(zipData)

	digest := sha256.Sum256(signingInput.Bytes())
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, err
	}

	// AsymmetricKeyProof{ public_key, signature }
	proof := append(protoBytes(1, pubDER), protoBytes(2, signature)...)

	// CrxFileHeader{ sha256_with_rsa=proof (field 2), signed_header_data (field 10000) }
	header := append(protoBytes(2, proof), protoBytes(10000, signedHeaderData)...)

	out := bytes.NewBuffer(nil)
	out.WriteString("Cr24")
	_ = binary.Write(out, binary.LittleEndian, uint32(3))
	_ = binary.Write(out, binary.LittleEndian, uint32(len(header)))
	out.Write(header)
	out.Write(zipData)
	return out.Bytes(), nil
}

// protoBytes encodes a protobuf length-delimited (wire type 2) field.
func protoBytes(fieldNum int, data []byte) []byte {
	var buf []byte
	buf = appendUvarint(buf, uint64(fieldNum)<<3|2)
	buf = appendUvarint(buf, uint64(len(data)))
	return append(buf, data...)
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func buildUpdateXML(id, version, crxURL string) []byte {
	xml := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8'?>
<gupdate xmlns='http://www.google.com/update2/response' protocol='2.0'>
  <app appid='%s'>
    <updatecheck codebase='%s' version='%s' />
  </app>
</gupdate>
`, id, crxURL, version)
	return []byte(xml)
}
