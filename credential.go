package goparcellocker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
)

// hintLen 是交接记录里凭证短标签的十六进制长度（取自摘要前缀）。
const hintLen = 12

// base32Lower 用于生成无歧义、便于人工输入的凭证字符集。
var base32Lower = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// randomToken 生成 n 字节随机熵、base32 编码的短期凭证明文。
// 明文只返回给调用方一次，系统内仅保存其 SHA-256 摘要。
func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32Lower.EncodeToString(b), nil
}

// randomID 生成预约 ID（12 字节随机熵的十六进制）。
func randomID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// credentialFingerprint 返回凭证明文的安全摘要。
//
// 使用以服务端密钥为键的 HMAC，而非裸 SHA-256：即使摘要泄露，
// 不掌握密钥也无法对猜测的凭证做离线比对。
func credentialFingerprint(secret []byte, token string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("credential/v1:"))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// fingerprintHint 返回摘要的短标签，用于交接记录人工对账；前缀长度不足以还原明文。
func fingerprintHint(fp string) string {
	if len(fp) > hintLen {
		return fp[:hintLen]
	}
	return fp
}

// derivePickupCode 由投递凭证与服务端 HMAC 密钥确定性派生存取件码。
//
// 投递凭证只在成功消费时出现一次；当同一投递作为幂等请求重放时，系统需要
// “返回原结果、凭证相同”，而明文并不入库。以投递凭证为输入做 HMAC 派生，
// 使得取件码无需保存明文也能在重放时稳定重现；攻击者不掌握服务端密钥，
// 仅凭入库的 SHA-256 摘要无法推算出取件码。
func derivePickupCode(hmacKey []byte, deliveryToken string) string {
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte("pickup-code/v1:"))
	mac.Write([]byte(deliveryToken))
	// 截断到 15 字节 -> 24 个 base32 字符，输入仍有 120 bit 熵。
	return base32Lower.EncodeToString(mac.Sum(nil)[:15])
}

// contentFingerprint 对调用方声明的包裹内容做归一化摘要：
// 同时覆盖 ContentHash 与尺寸，尺寸变化同样视为内容冲突。
func contentFingerprint(pkg Package) string {
	h := sha256.New()
	h.Write([]byte{byte(pkg.Size)})
	h.Write(pkg.ContentHash)
	return hex.EncodeToString(h.Sum(nil))
}

// constantTimeEqual 以常量时间比较两个十六进制摘要，避免计时侧信道。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func errWrapf(err error, format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
}
