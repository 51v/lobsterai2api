// index.go 内嵌控制台前端页面。
package console

import _ "embed"

//go:embed index.html
var indexHTMLRaw []byte

func indexHTML() ([]byte, error) {
	return indexHTMLRaw, nil
}
