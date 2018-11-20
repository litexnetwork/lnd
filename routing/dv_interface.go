package routing

type DvRouter interface {
	handleRequest(req interface{})

	handleResponse(res interface{})

	handleProbe(p interface{})

	handleLinkChange(lc interface{})

	FindPath(dst interface{})
}
