package recorder

import "sync"

type redactionAudit struct {
	mu       sync.Mutex
	request  RedactionScopeInfo
	response RedactionScopeInfo
	errors   int64
	rawTrace int64
	failures [2]protectionFailure
}

type protectionFailure struct {
	first error
	count int64
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
	defer a.mu.Unlock()

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
}

func (a *redactionAudit) addProtection(direction BodyDirection, mode ProtectionMode, reason string) {
	if a == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	s := a.scope(direction)
	if s.Protection == nil {
		s.Protection = &ProtectionCounts{}
	}

	switch mode {
	case ProtectionEncrypt:
		s.Protection.Encrypted++

	case ProtectionTokenize:
		s.Protection.Tokenized++

	default:
		s.Protection.Redacted++
	}

	if reason != "" {
		if s.Protection.Fallbacks == nil {
			s.Protection.Fallbacks = make(map[string]int64)
		}

		s.Protection.Fallbacks[reason]++
	}
}

func (a *redactionAudit) addProtectionFailure(direction BodyDirection, err error, count int64) {
	if a == nil || err == nil || count <= 0 {
		return
	}

	index := 0
	if direction == ResponseBody {
		index = 1
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.failures[index].first == nil {
		a.failures[index].first = err
	}

	a.failures[index].count += count
}

func (a *redactionAudit) protectionFailures() [2]protectionFailure {
	if a == nil {
		return [2]protectionFailure{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.failures
}

func (a *redactionAudit) addError(n int64) {
	if a == nil || n <= 0 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.errors += n
}

func (a *redactionAudit) addRawTrace(n int64) {
	if a == nil || n <= 0 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.rawTrace += n
}

func (a *redactionAudit) setBody(direction BodyDirection, body BodyRedactionInfo) {
	if a == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	copy := body
	if body.Protection != nil {
		protection := cloneProtectionCounts(*body.Protection)
		copy.Protection = &protection
	}

	a.scope(direction).Body = &copy
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

		if request.Protection != nil {
			protection := cloneProtectionCounts(*request.Protection)
			request.Protection = &protection
		}

		info.Request = &request
	}

	if !redactionScopeEmpty(a.response) {
		response := a.response
		if response.Body != nil {
			body := *response.Body
			response.Body = &body
		}

		if response.Protection != nil {
			protection := cloneProtectionCounts(*response.Protection)
			response.Protection = &protection
		}

		info.Response = &response
	}

	if info.Request == nil && info.Response == nil && info.Errors == 0 && info.RawTrace == 0 {
		return nil
	}

	return info
}

func redactionScopeEmpty(s RedactionScopeInfo) bool {
	return s.URL == 0 && s.Headers == 0 && s.QueryParameters == 0 && s.Cookies == 0 && s.Body == nil && s.Protection == nil
}
