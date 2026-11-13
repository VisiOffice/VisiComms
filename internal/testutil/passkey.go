package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

var b64 = base64.RawURLEncoding

// SoftPasskey is a software authenticator holding discoverable credentials:
// ES256 keys, packed self-attestation, user verification whenever the site
// requires it. Adapted from VisiGrid's test helper. Options and responses
// are the JSON the browser page exchanges with the API.
type SoftPasskey struct {
	creds   []softCred
	counter uint32
}

type softCred struct {
	id, userHandle []byte
	key            *ecdsa.PrivateKey
}

func NewSoftPasskey() *SoftPasskey { return &SoftPasskey{} }

func clientData(kind, challenge, origin string) []byte {
	data, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return data
}

func publicKey(options any) (map[string]any, error) {
	raw, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	pk, ok := m["publicKey"].(map[string]any)
	if !ok {
		return nil, errors.New("options.publicKey missing")
	}
	return pk, nil
}

// Register answers navigator.credentials.create(options) at origin.
func (s *SoftPasskey) Register(origin string, options any) (json.RawMessage, error) {
	pk, err := publicKey(options)
	if err != nil {
		return nil, err
	}
	rp, _ := pk["rp"].(map[string]any)
	rpID, _ := rp["id"].(string)
	challenge, _ := pk["challenge"].(string)
	user, _ := pk["user"].(map[string]any)
	handleB64, _ := user["id"].(string)
	handle, err := b64.DecodeString(handleB64)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	pub, _ := key.PublicKey.ECDH()
	raw := pub.Bytes() // 0x04 || X || Y
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: -7},
		Curve:         1, XCoord: raw[1:33], YCoord: raw[33:65]})
	if err != nil {
		return nil, err
	}
	rpHash := sha256.Sum256([]byte(rpID))
	auth := append([]byte{}, rpHash[:]...)
	auth = append(auth, 0x45, 0, 0, 0, 0) // user present, verified, attested data
	auth = append(auth, make([]byte, 16)...)
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(id)))
	auth = append(auth, id...)
	auth = append(auth, cose...)
	cd := clientData("webauthn.create", challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, err
	}
	object, err := webauthncbor.Marshal(map[string]any{
		"fmt": "packed", "attStmt": map[string]any{"alg": -7, "sig": sig}, "authData": auth})
	if err != nil {
		return nil, err
	}
	s.creds = append(s.creds, softCred{id: id, userHandle: handle, key: key})
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(id), "rawId": b64.EncodeToString(id), "type": "public-key",
		"response":   map[string]any{"attestationObject": b64.EncodeToString(object), "clientDataJSON": b64.EncodeToString(cd)},
		"extensions": map[string]any{},
	})
}

// Authenticate answers a discoverable navigator.credentials.get(options) at
// origin with the most recently registered credential.
func (s *SoftPasskey) Authenticate(origin string, options any) (json.RawMessage, error) {
	pk, err := publicKey(options)
	if err != nil {
		return nil, err
	}
	if len(s.creds) == 0 {
		return nil, errors.New("no credential")
	}
	c := s.creds[len(s.creds)-1]
	rpID, _ := pk["rpId"].(string)
	challenge, _ := pk["challenge"].(string)
	s.counter++
	rpHash := sha256.Sum256([]byte(rpID))
	auth := append(append([]byte{}, rpHash[:]...), 0x05) // user present, verified
	auth = binary.BigEndian.AppendUint32(auth, s.counter)
	cd := clientData("webauthn.get", challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, c.key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(c.id), "rawId": b64.EncodeToString(c.id), "type": "public-key",
		"response": map[string]any{"authenticatorData": b64.EncodeToString(auth), "clientDataJSON": b64.EncodeToString(cd),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(c.userHandle)},
		"extensions": map[string]any{},
	})
}
