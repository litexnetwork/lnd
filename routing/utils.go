package routing

import (
	"bytes"
	"github.com/roasbeef/btcutil"
	"time"
	"crypto/md5"
	"encoding/hex"
	"strconv"
)

func IfKeyEqual(a, b[33]byte)  bool{
	return bytes.Equal(a[:], b[:])
}

func MinAmount(a, b btcutil.Amount) btcutil.Amount {
	if a < b {
		return a
	} else {
		return b
	}
}

func GenRequestID(s string) string {
	str := MD5(strconv.FormatInt(time.Now().UnixNano(),10) + s)
	return "0" + str
}

func MD5(text string) string {
	ctx := md5.New()
	ctx.Write([]byte(text))
	return hex.EncodeToString(ctx.Sum(nil))
}

