package dkeyczar

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/ioutil"
	"math/big"
	"os"
	"strconv"
)

// KeyReader provides an interface for returning information about a particular key.
type KeyReader interface {
	// GetMetadata returns the meta information for this key
	GetMetadata() (string, error)
	// GetKey returns the key material for a particular version of this key
	GetKey(version int) (string, error)
}

type fileReader struct {
	location string // directory path of keyfiles
}

// NewFileReader returns a KeyReader that reads a keyczar key from a directory on the file system.
func NewFileReader(location string) KeyReader {
	r := new(fileReader)

	// make sure 'location' ends with our path separator
	if location[len(location)-1] == os.PathSeparator {
		r.location = location
	} else {
		r.location = location + string(os.PathSeparator)
	}

	return r
}

// return the entire contents of a file as a string
func slurp(path string) (string, error) {
	b, err := ioutil.ReadFile(path)
	return string(b), err
}

// slurp and return the meta file
func (r *fileReader) GetMetadata() (string, error) {
	return slurp(r.location + "meta")
}

// slurp and return the requested key version
func (r *fileReader) GetKey(version int) (string, error) {
	return slurp(r.location + strconv.Itoa(version))
}

type encryptedReader struct {
	reader  KeyReader // our wrapped reader
	crypter Crypter   // the crypter we use to decrypt what we've read
}

// NewEncryptedReader returns a KeyReader which decrypts the key returned by the wrapped 'reader'.
func NewEncryptedReader(reader KeyReader, crypter Crypter) KeyReader {
	r := new(encryptedReader)

	r.crypter = crypter
	r.reader = reader

	return r
}

// return the meta information from the wrapper reader.  Meta information is not encrypted.
func (r *encryptedReader) GetMetadata() (string, error) {
	return r.reader.GetMetadata()
}

// decrypt and return an encrypted key
func (r *encryptedReader) GetKey(version int) (string, error) {
	s, err := r.reader.GetKey(version)

	if err != nil {
		return "", err

	}

	b, err := r.crypter.Decrypt(s)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

type pbeReader struct {
	reader   KeyReader // our wrapped reader
	password []byte    // the password to use for the PBE
}

// NewPBEReader returns a KeyReader which decrypts keys encrypted with password-based encryption
func NewPBEReader(reader KeyReader, password []byte) KeyReader {
	r := new(pbeReader)

	r.password = make([]byte, len(password))
	copy(r.password, password)
	r.reader = reader

	// FIXME: double check that the reader is looking at an encrypted key?

	return r
}

// return the meta information from the wrapper reader.  Meta information is not encrypted.
func (r *pbeReader) GetMetadata() (string, error) {
	return r.reader.GetMetadata()
}

type pbeKeyJSON struct {
	Cipher         string `json:"cipher"`
	Hmac           string `json:"hmac"`
	IterationCount int    `json:"iterationCount"`
	Iv             string `json:"iv"`
	Key            string `json:"key"`
	Salt           string `json:"salt"`
}

// decrypt and return an encrypted key
func (r *pbeReader) GetKey(version int) (string, error) {
	s, err := r.reader.GetKey(version)

	if err != nil {
		return "", err

	}

	var pbejson pbeKeyJSON

	json.Unmarshal([]byte(s), &pbejson)

	if pbejson.Cipher != "AES128" || pbejson.Hmac != "HMAC_SHA1" {
		return "", ErrUnsupportedType
	}

	salt, _ := decodeWeb64String(pbejson.Salt)
	iv_bytes, _ := decodeWeb64String(pbejson.Iv)
	ciphertext, _ := decodeWeb64String(pbejson.Key)

	keybytes := pbkdf2(r.password, salt, pbejson.IterationCount, 128/8)

	aesCipher, _ := aes.NewCipher(keybytes)

	crypter := cipher.NewCBCDecrypter(aesCipher, iv_bytes)

	plaintext := make([]byte, len(ciphertext))

	crypter.CryptBlocks(plaintext, ciphertext)

	return string(plaintext), nil
}

// a fake reader for an RSA private key
type importedRsaPrivateKeyReader struct {
	km      keyMeta    // our fake meta info
	rsajson rsaKeyJSON // the rsa key we're importing
}

// construct a fake keyreader for the provided rsa private key and purpose
func newImportedRsaPrivateKeyReader(key *rsa.PrivateKey, purpose keyPurpose) KeyReader {
	r := new(importedRsaPrivateKeyReader)
	kv := keyVersion{1, ksPRIMARY, false}
	r.km = keyMeta{"Imported RSA Private Key", ktRSA_PRIV, purpose, false, []keyVersion{kv}}

	// inverse of code with newRsaKeys
	r.rsajson.PublicKey.Modulus = encodeWeb64String(key.PublicKey.N.Bytes())

	e := big.NewInt(int64(key.PublicKey.E))
	r.rsajson.PublicKey.PublicExponent = encodeWeb64String(e.Bytes())

	r.rsajson.PrimeP = encodeWeb64String(key.Primes[0].Bytes())
	r.rsajson.PrimeQ = encodeWeb64String(key.Primes[1].Bytes())
	r.rsajson.PrivateExponent = encodeWeb64String(key.D.Bytes())
	r.rsajson.PrimeExponentP = encodeWeb64String(key.Precomputed.Dp.Bytes())
	r.rsajson.PrimeExponentQ = encodeWeb64String(key.Precomputed.Dq.Bytes())
	r.rsajson.CrtCoefficient = encodeWeb64String(key.Precomputed.Qinv.Bytes())

	return r
}

func (r *importedRsaPrivateKeyReader) GetMetadata() (string, error) {
	b, err := json.Marshal(r.km)
	return string(b), err
}

func (r *importedRsaPrivateKeyReader) GetKey(version int) (string, error) {
	b, err := json.Marshal(r.rsajson)
	return string(b), err
}

// load and return an rsa private key from a PEM file specified in 'location'
func getRsaKeyFromPem(location string) (*rsa.PrivateKey, error) {

	buf, err := slurp(location)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode([]byte(buf))

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	return priv, nil
}

// ImportRSAKeyFromPEMForSigning returns a KeyReader for the RSA Private Key contained in the PEM file specified in the location.
// The resulting key can be used for signing and verification only
func ImportRSAKeyFromPEMForSigning(location string) (KeyReader, error) {

	priv, err := getRsaKeyFromPem(location)
	if err != nil {
		return nil, err
	}

	r := newImportedRsaPrivateKeyReader(priv, kpSIGN_AND_VERIFY)

	return r, nil
}

// ImportRSAKeyFromPEMForCrypt returns a KeyReader for the RSA Private Key contained in the PEM file specified in the location.
// The resulting key can be used for encryption and decryption only
func ImportRSAKeyFromPEMForCrypt(location string) (KeyReader, error) {

	priv, err := getRsaKeyFromPem(location)
	if err != nil {
		return nil, err
	}

	r := newImportedRsaPrivateKeyReader(priv, kpDECRYPT_AND_ENCRYPT)

	return r, nil
}

// a fake reader for an RSA public key
type importedRsaPublicKeyReader struct {
	km      keyMeta          // our fake meta info
	rsajson rsaPublicKeyJSON // the rsa key we're importing
}

// construct a fake keyreader for the provided rsa public key and purpose
func newImportedRsaPublicKeyReader(key *rsa.PublicKey, purpose keyPurpose) KeyReader {
	r := new(importedRsaPublicKeyReader)
	kv := keyVersion{1, ksPRIMARY, false}
	r.km = keyMeta{"Imported RSA Public Key", ktRSA_PUB, purpose, false, []keyVersion{kv}}

	// inverse of code with newRsaKeys
	r.rsajson.Modulus = encodeWeb64String(key.N.Bytes())

	e := big.NewInt(int64(key.E))
	r.rsajson.PublicExponent = encodeWeb64String(e.Bytes())

	return r
}

func (r *importedRsaPublicKeyReader) GetMetadata() (string, error) {
	b, err := json.Marshal(r.km)
	return string(b), err
}

func (r *importedRsaPublicKeyReader) GetKey(version int) (string, error) {
	b, err := json.Marshal(r.rsajson)
	return string(b), err
}

// load and return an rsa public key from a PEM file specified in 'location'
func getRsaPublicKeyFromPem(location string) (*rsa.PublicKey, error) {

	buf, err := slurp(location)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode([]byte(buf))

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	rsapub, ok := pub.(*rsa.PublicKey)

	if !ok {
		// FIXME: lousy error message :(
		return nil, ErrUnsupportedType
	}

	return rsapub, nil
}

// ImportRSAPublicKeyFromPEM returns a KeyReader for the RSA Public Key contained in the PEM file specified in the location.
// The resulting key can be used for encryption only.
func ImportRSAPublicKeyFromPEMForEncryption(location string) (KeyReader, error) {

	rsapub, err := getRsaPublicKeyFromPem(location)
	if err != nil {
		return nil, err
	}
	r := newImportedRsaPublicKeyReader(rsapub, kpENCRYPT)

	return r, nil
}

// ImportRSAPublicKeyFromPEMForVerify returns a KeyReader for the RSA Public Key contained in the PEM file specified in the location.
// The resulting key can be used for verification only.
func ImportRSAPublicKeyFromPEMForVerify(location string) (KeyReader, error) {

	rsapub, err := getRsaPublicKeyFromPem(location)
	if err != nil {
		return nil, err
	}
	r := newImportedRsaPublicKeyReader(rsapub, kpVERIFY)

	return r, nil
}

func getRsaPublicKeyFromCertificate(location string) (*rsa.PublicKey, error) {

	buf, err := slurp(location)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode([]byte(buf))

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	rsapub, ok := cert.PublicKey.(*rsa.PublicKey)

	if !ok {
		// FIXME: lousy error message :(
		return nil, ErrUnsupportedType
	}

	return rsapub, nil
}

// ImportRSAPublicKeyFromCertificateForVerify returns a KeyReader for the RSA Public Key contained in the certificate file specified in the location.
// The resulting key can be used for verification only.
func ImportRSAPublicKeyFromCertificateForVerify(location string) (KeyReader, error) {

	rsapub, err := getRsaPublicKeyFromCertificate(location)
	if err != nil {
		return nil, err
	}
	r := newImportedRsaPublicKeyReader(rsapub, kpVERIFY)

	return r, nil
}

// ImportRSAPublicKeyFromCertificateForCrypt returns a KeyReader for the RSA Public Key contained in the certificate file specified in the location.
// The resulting key can be used for encryption only.
func ImportRSAPublicKeyFromCertificateForCrypt(location string) (KeyReader, error) {

	rsapub, err := getRsaPublicKeyFromCertificate(location)
	if err != nil {
		return nil, err
	}
	r := newImportedRsaPublicKeyReader(rsapub, kpENCRYPT)

	return r, nil
}

// fake reader for an AES key
type importedAesKeyReader struct {
	km      keyMeta    // our fake meta info
	aesjson aesKeyJSON // the aes key we're importing
}

// construct a fake keyreader for the provided aes key
func newImportedAesKeyReader(key *aesKey) KeyReader {
	r := new(importedAesKeyReader)
	kv := keyVersion{1, ksPRIMARY, false}
	r.km = keyMeta{"Imported AES Key", ktAES, kpDECRYPT_AND_ENCRYPT, false, []keyVersion{kv}}

	// inverse of code with newAesKeys
	r.aesjson.AesKeyString = encodeWeb64String(key.key)
	r.aesjson.Size = len(key.key) * 8
	r.aesjson.HmacKey.HmacKeyString = encodeWeb64String(key.hmacKey.key)
	r.aesjson.HmacKey.Size = len(key.hmacKey.key) * 8
	r.aesjson.Mode = cmCBC

	return r
}

func (r *importedAesKeyReader) GetMetadata() (string, error) {
	b, err := json.Marshal(r.km)
	return string(b), err
}

func (r *importedAesKeyReader) GetKey(version int) (string, error) {
	b, err := json.Marshal(r.aesjson)
	return string(b), err
}
