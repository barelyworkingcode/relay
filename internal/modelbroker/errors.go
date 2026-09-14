package modelbroker

// ErrorBody is a pre-rendered error response: the exact status and bytes to
// write, per spec §2.4. Reason is never sent on the wire — it is what the
// caller audits (spec §7's outcome field), so that a disallowed and an
// unknown model can produce byte-identical responses while still being
// distinguishable in `relay audit`.
type ErrorBody struct {
	Status int
	Body   []byte
	Reason string
}

const (
	openAIUnauthorized       = `{"error":{"message":"unauthorized","type":"authentication_error"}}`
	anthropicUnauthorized    = `{"type":"error","error":{"type":"authentication_error","message":"unauthorized"}}`
	openAIModelNotFound      = `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`
	anthropicModelNotFound   = `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`
	openAIRouteNotFound      = `{"error":{"message":"route not found","type":"invalid_request_error","code":"model_not_found"}}`
	anthropicRouteNotFound   = `{"type":"error","error":{"type":"not_found_error","message":"route not found"}}`
	openAIRemoteProject      = `{"error":{"message":"permission denied","type":"permission_error"}}`
	anthropicRemoteProject   = `{"type":"error","error":{"type":"permission_error","message":"permission denied"}}`
	openAIHostUnavailable    = `{"error":{"message":"model host unavailable","type":"server_error"}}`
	anthropicHostUnavailable = `{"type":"error","error":{"type":"api_error","message":"model host unavailable"}}`
)

// UnauthorizedError is the 401 for a missing or unrecognised credential, and
// for tokenless TCP — spec §2.4 says every 401 is identical, so this takes
// no Reason parameter; the caller supplies its own (e.g. "unauthorized") to
// audit.
func UnauthorizedError(shape Shape) ErrorBody {
	body := openAIUnauthorized
	if shape == ShapeAnthropic {
		body = anthropicUnauthorized
	}
	return ErrorBody{Status: 401, Body: []byte(body)}
}

// ModelNotFoundError is the 404 for a model that Allowed refused, whether
// because the grant does not cover it (ReasonDenied) or because it does not
// exist in the catalog under any accepted spelling (ReasonNotFound). Both
// reasons render the identical body — spec §2.4: "deliberately the same
// 404, so a project cannot enumerate models outside its grant" — reason is
// carried only for the audit record.
func ModelNotFoundError(shape Shape, reason string) ErrorBody {
	body := openAIModelNotFound
	if shape == ShapeAnthropic {
		body = anthropicModelNotFound
	}
	return ErrorBody{Status: 404, Body: []byte(body), Reason: reason}
}

// RouteNotFoundError is the 404 for a method+path MatchRoute refused.
func RouteNotFoundError(shape Shape) ErrorBody {
	body := openAIRouteNotFound
	if shape == ShapeAnthropic {
		body = anthropicRouteNotFound
	}
	return ErrorBody{Status: 404, Body: []byte(body), Reason: "route_not_found"}
}

// RemoteProjectError is the 403 for any call scoped to a remote project,
// whatever its allowed_models says (spec §3.3, §9).
func RemoteProjectError(shape Shape) ErrorBody {
	body := openAIRemoteProject
	if shape == ShapeAnthropic {
		body = anthropicRemoteProject
	}
	return ErrorBody{Status: 403, Body: []byte(body), Reason: "remote_project"}
}

// HostUnavailableError is the 503 for no registered model host, or one
// relay could not reach.
func HostUnavailableError(shape Shape) ErrorBody {
	body := openAIHostUnavailable
	if shape == ShapeAnthropic {
		body = anthropicHostUnavailable
	}
	return ErrorBody{Status: 503, Body: []byte(body), Reason: "host_unavailable"}
}
