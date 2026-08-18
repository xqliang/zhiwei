package pipeline

import "fmt"

func errNoHandler(stage string) error { return fmt.Errorf("stage %s 未注册 handler", stage) }
