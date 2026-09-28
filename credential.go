package goparcellocker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// 凭证以 "dlv_"/"pck_" 前缀加随机字节生成；系统内部只保存其 HMAC 摘要，
// 明文只在创建/派生时返回一次。取件凭证由投递凭证确定性派生，使得同一投递
// 凭证的幂等重放返回完全相同的取件凭证。
const (
	deliveryPrefix = "dlv_"
	pickupPrefix   = "pck_"
)

// newToken 生成形如 prefix + base64url(random) 的随机凭证明文。
func newToken(prefix string, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// digestKey 用于对凭证做带密钥的 HMAC 摘要，避免系统保存任何凭证明文。
type digestKey []byte

// tokenDigest 返回凭证明文的十六进制 HMAC-SHA256 摘要。
func (k digestKey) tokenDigest(token string) string {
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// equalDigest 使用恒定时间比较两个摘要。
func equalDigest(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pickupToken 由投递凭证确定性派生取件凭证。派生命名空间与凭证摘要分离，
// 因此取件凭证的摘要不会等于投递凭证摘要，两个索引互不串扰。
func pickupToken(deliveryToken string, key digestKey) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("pickup-derive-v1:"))
	mac.Write([]byte(deliveryToken))
	return pickupPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parcelDigest 是包裹信息的安全摘要，用于在投递凭证重放时判定“同一内容”
// 还是“凭证相同但包裹信息变化”的冲突。
type parcelDigest string

// digestParcel 以规范序列化计算包裹信息摘要（同样的包裹字段 → 同样的摘要）。
func digestParcel(p ParcelInfo, key digestKey) parcelDigest {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "tracking=%s\nsize=%d\nweight=%d\nsender=%s\nrecipient=%s\n",
		p.TrackingNumber, p.Size, p.WeightGrams, p.Sender, p.Recipient)
	return parcelDigest(hex.EncodeToString(mac.Sum(nil)))
}
