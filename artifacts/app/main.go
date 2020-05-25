package main

import (
	"flag"
	"github.com/gin-gonic/gin"
	"net/http"
	"os"
)

var addr = flag.String("addr", ":8080", "address")

func main() {
	flag.Parse()
	r := gin.New()
	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "hello %s this message from %s/%s", c.Query("name"), os.Getenv("POD"), os.Getenv("NODE"))
	})
	r.Run(*addr)
}
