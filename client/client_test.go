package client

import (
	"fmt"
	"net/url"
	"testing"
)

func Test(t *testing.T) {
	v := url.Values{}
	v.Set("mail_id", "10086")
	v.Set("mail_name", "suilz")
	v.Set("mail_time", "2019-06-10")

	reqParams := v.Encode()
	env := make(map[string]string)

	env["REQUEST_METHOD"] = "GET"
	env["SCRIPT_FILENAME"] = "/home/code/php/index.php"
	env["SERVER_SOFTWARE"] = "golang"
	env["REMOTE_ADDR"] = "127.0.0.1"
	env["SERVER_NAME"] = "127.0.0.1"
	env["SERVER_PROTOCOL"] = "HTTP/1.1"
	env["REQUEST_URI"] = "/"
	env["QUERY_STRING"] = reqParams
	fcgi, err := NewClient("192.168.100.7:9000") //PHP-FPM
	// fcgi, err := NewClient("127.0.0.1:9001") //golang fcgi server
	if err != nil {
		fmt.Printf("err: %v", err)
	}
	// fmt.Printf("%v", reqParams)
	// reqParams = ""

	retout, reterr, err := fcgi.Request(env, reqParams)
	if err != nil {
		fmt.Printf("err: %v", err)
	}
	fmt.Printf("retout: %s", retout)
	fmt.Printf("reterr: %s", reterr)
}
