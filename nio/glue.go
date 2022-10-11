package nio

// CloseWriter is one of possible interfaces implemented by Out to send a FIN, without closing
// the input. Some writers only do this when Close is called.
type CloseWriter interface {
	CloseWrite() error
}
