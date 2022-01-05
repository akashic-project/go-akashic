//go:build go1.4
// +build go1.4

package log

import "sync/atomic"

// swapHandler wraps another handler that may be swapped out
// dynamically at runtime in a thread-safe fashion.
// swapHandlerは、実行時にスレッドセーフな方法で動的にスワップアウトされる可能性のある別のハンドラーをラップします。
type swapHandler struct {
	handler atomic.Value
}

func (h *swapHandler) Log(r *Record) error {
	return (*h.handler.Load().(*Handler)).Log(r)
}

func (h *swapHandler) Swap(newHandler Handler) {
	h.handler.Store(&newHandler)
}

func (h *swapHandler) Get() Handler {
	return *h.handler.Load().(*Handler)
}
