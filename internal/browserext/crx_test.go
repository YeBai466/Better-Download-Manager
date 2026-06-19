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
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func sampleFS() fstest.MapFS {
	return fstest.MapFS{
		"manifest.json": {Data: []byte(`{"manifest_version":3,"name":"x","version":"2.3.4"}`)},
		"background.js": {Data: []byte("console.log('hi')")},
	}
}

func TestBuildProducesValidCRX(t *testing.T) {
	keyDir := t.TempDir()
	b, err := Build(sampleFS(), keyDir, "http://127.0.0.1:9614/ext")
	if err != nil {
		t.Fatal(err)
	}

	// ID is 32 chars in the a-p alphabet.
	if len(b.ID) != 32 {
		t.Fatalf("id length = %d", len(b.ID))
	}
	for _, c := range b.ID {
		if c < 'a' || c > 'p' {
			t.Fatalf("id has invalid char %q", c)
		}
	}
	if b.Version != "2.3.4" {
		t.Fatalf("version = %s", b.Version)
	}

	// CRX header: magic + version 3.
	if string(b.CRX[:4]) != "Cr24" {
		t.Fatalf("bad magic %q", b.CRX[:4])
	}
	if v := binary.LittleEndian.Uint32(b.CRX[4:8]); v != 3 {
		t.Fatalf("bad crx version %d", v)
	}
	headerSize := binary.LittleEndian.Uint32(b.CRX[8:12])
	zipStart := 12 + int(headerSize)
	zipData := b.CRX[zipStart:]

	// The trailing payload is a valid zip containing manifest.json.
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("manifest.json missing from crx zip")
	}

	// Recreate the signature deterministically (PKCS1v15) and confirm it is
	// embedded in the CRX, proving the signing path is correct.
	key := loadKey(t, keyDir)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	crxID := crxIDFromPubKey(pubDER)
	if encodeExtensionID(crxID) != b.ID {
		t.Fatal("id does not match signing key")
	}
	signedHeaderData := protoBytes(1, crxID)
	si := bytes.NewBuffer(nil)
	si.WriteString("CRX3 SignedData\x00")
	_ = binary.Write(si, binary.LittleEndian, uint32(len(signedHeaderData)))
	si.Write(signedHeaderData)
	si.Write(zipData)
	digest := sha256.Sum256(si.Bytes())
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.CRX[:zipStart], sig) {
		t.Fatal("signature not found in crx header")
	}

	// update.xml references the id, crx url and version.
	xml := string(b.UpdateXML)
	for _, want := range []string{b.ID, "bdm.crx", b.Version} {
		if !strings.Contains(xml, want) {
			t.Fatalf("update.xml missing %q: %s", want, xml)
		}
	}
}

func TestKeyIsStable(t *testing.T) {
	keyDir := t.TempDir()
	b1, err := Build(sampleFS(), keyDir, "http://127.0.0.1:9614/ext")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Build(sampleFS(), keyDir, "http://127.0.0.1:9614/ext")
	if err != nil {
		t.Fatal(err)
	}
	if b1.ID != b2.ID {
		t.Fatalf("id not stable: %s vs %s", b1.ID, b2.ID)
	}
}

func loadKey(t *testing.T, keyDir string) *rsa.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(keyDir, "ext_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
