package cli

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// console serialises terminal input between the chat prompt and approval
// questions: one goroutine reads lines, whoever is asking consumes them.
type console struct {
	once  sync.Once
	lines chan string
}

func (c *console) next(cancel <-chan struct{}) (string, bool) {
	c.once.Do(func() {
		c.lines = make(chan string)
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				c.lines <- sc.Text()
			}
			close(c.lines)
		}()
	})
	select {
	case l, ok := <-c.lines:
		return strings.TrimSpace(l), ok
	case <-cancel:
		return "", false
	}
}
