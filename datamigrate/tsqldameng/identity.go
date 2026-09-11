package tsqldameng

import (
	"fmt"
	"math/big"
)

// IdentityNext preserves the source progression while stepping beyond imported
// extremes. big.Int avoids overflowing at the ends of the BIGINT range.
func IdentityNext(seed, step int64, last, low, high *int64) (int64, error) {
	if step == 0 {
		return 0, fmt.Errorf("自增步长为零")
	}
	n := big.NewInt(seed)
	inc := big.NewInt(step)
	if last != nil {
		n.Add(big.NewInt(*last), inc)
	}
	bound := high
	if step < 0 {
		bound = low
	}
	if bound != nil {
		b := big.NewInt(*bound)
		if step > 0 && n.Cmp(b) <= 0 || step < 0 && n.Cmp(b) >= 0 {
			distance := new(big.Int).Sub(b, n)
			distance.Quo(distance, inc)
			distance.Add(distance, big.NewInt(1))
			n.Add(n, new(big.Int).Mul(distance, inc))
		}
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("自增下一值超出 BIGINT 范围")
	}
	return n.Int64(), nil
}
