package recorder

import "sync"

type redactionAudit struct {
	mu       sync.Mutex
	request  RedactionScopeInfo
	response RedactionScopeInfo
	errors   int64
	rawTrace int64
}

func (a *redactionAudit) scope(direction BodyDirection) *RedactionScopeInfo {
	if direction == ResponseBody {
		return &a.response
	}
	return &a.request
}

func (a *redactionAudit) add(direction BodyDirection, category string, n int64) {
	if a == nil || n <= 0 {
		return
	}
	a.mu.Lock()
	s := a.scope(direction)
	switch category {
	case "url":
		s.URL += n
	case "headers":
		s.Headers += n
	case "query":
		s.QueryParameters += n
	case "cookies":
		s.Cookies += n
	}
	a.mu.Unlock()
}

func (a *redactionAudit) addError(n int64) {
	if a == nil || n <= 0 {
		return
	}
	a.mu.Lock()
	a.errors += n
	a.mu.Unlock()
}

func (a *redactionAudit) addRawTrace(n int64) {
	if a == nil || n <= 0 {
		return
	}
	a.mu.Lock()
	a.rawTrace += n
	a.mu.Unlock()
}

func (a *redactionAudit) setBody(direction BodyDirection, body BodyRedactionInfo) {
	if a == nil {
		return
	}
	a.mu.Lock()
	copy := body
	a.scope(direction).Body = &copy
	a.mu.Unlock()
}

func (a *redactionAudit) snapshot() *RedactionInfo {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info := &RedactionInfo{Errors: a.errors, RawTrace: a.rawTrace}
	if !redactionScopeEmpty(a.request) {
		request := a.request
		if request.Body != nil {
			body := *request.Body
			request.Body = &body
		}
		info.Request = &request
	}
	if !redactionScopeEmpty(a.response) {
		response := a.response
		if response.Body != nil {
			body := *response.Body
			response.Body = &body
		}
		info.Response = &response
	}
	if info.Request == nil && info.Response == nil && info.Errors == 0 && info.RawTrace == 0 {
		return nil
	}
	return info
}

func redactionScopeEmpty(s RedactionScopeInfo) bool {
	return s.URL == 0 && s.Headers == 0 && s.QueryParameters == 0 && s.Cookies == 0 && s.Body == nil
}
