package app

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// randomDigits 生成 n 位随机数字串，用于图形验证码。
//
// 使用 crypto/rand 而非 math/rand，避免验证码可被预测。
func randomDigits(n int) string {
	const digits = "0123456789"
	var b strings.Builder
	b.Grow(n)
	max := big.NewInt(int64(len(digits)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 失败属于系统级异常，退化为固定值也要保证不 panic。
			b.WriteByte('0')
			continue
		}
		b.WriteByte(digits[idx.Int64()])
	}
	return b.String()
}

// renderCaptchaSVG 把验证码文本渲染为 SVG。
//
// 设计说明：不引入图形库，用旋转 + 随机基线的字符直接拼 SVG。
// 对「阻止脚本暴力登录」的目标足够，且没有额外依赖与体积开销。
// 每个字符的旋转角由位置推导（而非随机数），保证同样的 code 产生稳定的图形。
func renderCaptchaSVG(code string) string {
	const (
		width  = 120
		height = 40
	)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`,
		width, height, width, height)
	// 背景
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#f2f4f7"/>`, width, height)

	// 干扰线：三条固定斜线，位置随验证码长度偏移，避免完全静态。
	for i := 0; i < 3; i++ {
		x1 := 6 + i*13 + len(code)
		y1 := 5 + i*11
		x2 := width - 6 - i*9
		y2 := height - 5 - i*7
		fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#c9cdd4" stroke-width="1"/>`,
			x1, y1, x2, y2)
	}

	// 字符：逐个旋转，增加 OCR 难度。
	step := (width - 24) / max1(len(code))
	for i, ch := range code {
		x := 14 + i*step
		// 旋转角在 -25° ~ 25° 之间按位置摆动，基线上下浮动 ±4px。
		angle := (i%3)*18 - 18
		y := height/2 + 8 + ((i%2)*8 - 4)
		fmt.Fprintf(&b,
			`<text x="%d" y="%d" font-family="monospace" font-size="24" font-weight="700" fill="#1f2329" transform="rotate(%d %d %d)">%c</text>`,
			x, y, angle, x, y, ch)
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// max1 返回不小于 1 的值，避免除零。
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
