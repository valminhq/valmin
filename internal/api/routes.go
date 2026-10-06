package api

import "net/http"

type routeSpec struct {
	pattern   string
	handler   http.Handler
	stream    bool
	largeBody bool
}

type routeTable struct {
	routes []routeSpec
}

func (t *routeTable) Handle(pattern string, handler http.Handler) {
	t.routes = append(t.routes, routeSpec{pattern: pattern, handler: handler})
}

func (t *routeTable) Stream(pattern string, handler http.Handler) {
	t.routes = append(t.routes, routeSpec{pattern: pattern, handler: handler, stream: true})
}

func (t *routeTable) Large(pattern string, handler http.Handler) {
	t.routes = append(t.routes, routeSpec{pattern: pattern, handler: handler, largeBody: true})
}

func (t *routeTable) LargeStream(pattern string, handler http.Handler) {
	t.routes = append(t.routes, routeSpec{pattern: pattern, handler: handler, stream: true, largeBody: true})
}

func (t *routeTable) install(router *Router) map[string]bool {
	large := make(map[string]bool)
	for _, route := range t.routes {
		if route.stream {
			router.stream(route.pattern, route.handler)
		} else {
			router.handle(route.pattern, route.handler)
		}
		if route.largeBody {
			large[route.pattern] = true
		}
	}
	return large
}
