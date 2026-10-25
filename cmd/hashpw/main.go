package main

import (
	"fmt"
	"os"

	biz "github.com/looplj/axonhub/internal/server/biz"
)

func main() {
	hash, pw := os.Args[1], os.Args[2]
	if err := biz.VerifyPassword(hash, pw); err != nil {
		fmt.Println("VERIFY FAIL:", err)
		return
	}
	fmt.Println("VERIFY OK")
}
