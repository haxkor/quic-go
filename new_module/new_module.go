package new_module

import (
    "github.com/quic-go/quic-go/qlog"
)

func Foo() {
    if qlog.DefaultTracer != nil {
        return
    }
}
