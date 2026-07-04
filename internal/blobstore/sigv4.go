package blobstore

// AWS Signature Version 4 request signing, S3 flavor, stdlib only. The
// algorithm is specified in AWS's "Signature Version 4 signing process" docs
// and verified here against the published test vector (see sigv4_test.go).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// emptyPayloadHash is SHA-256 of zero bytes (GET/DELETE bodies).
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// unsignedPayload skips body hashing for streamed uploads.
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

// sign adds x-amz-date, x-amz-content-sha256, and Authorization to req. Every
// header already present on the request is included in the signature, so set
// all headers before calling.
func sign(req *http.Request, accessKey, secretKey, region, payloadHash string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers: every signed header, lowercased, sorted, with
	// trimmed values. Host travels outside req.Header in net/http.
	headers := map[string]string{"host": req.Host}
	if headers["host"] == "" {
		headers["host"] = req.URL.Host
	}
	for k, vs := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(vs, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		canonicalQuery(req.URL.RawQuery),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
			", SignedHeaders="+signedHeaders+
			", Signature="+signature)
}

// canonicalQuery re-encodes a query string the way SigV4 wants: parameters
// sorted by name, values AWS-percent-encoded.
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	type kv struct{ k, v string }
	decoded := make([]kv, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		ku, _ := unescape(k)
		vu, _ := unescape(v)
		decoded = append(decoded, kv{ku, vu})
	}
	sort.Slice(decoded, func(i, j int) bool {
		if decoded[i].k != decoded[j].k {
			return decoded[i].k < decoded[j].k
		}
		return decoded[i].v < decoded[j].v
	})
	parts := make([]string, 0, len(decoded))
	for _, p := range decoded {
		parts = append(parts, uriEncode(p.k, false)+"="+uriEncode(p.v, false))
	}
	return strings.Join(parts, "&")
}

// uriEncode is AWS's URI encoding: unreserved characters (A-Z a-z 0-9 - . _ ~)
// stay literal, everything else becomes %XX; keepSlash leaves path separators
// alone (object keys may contain them).
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// unescape decodes %XX sequences and '+' (query context).
func unescape(s string) (string, error) {
	s = strings.ReplaceAll(s, "+", " ")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.Write(v)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
