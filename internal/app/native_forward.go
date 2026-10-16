package app

import "encoding/json"

// The public API has messages/model/stream; the Hark website has message and
// Project routing. All other values go upstream without interpreting tool
// schemas. The selected Project-only behavior instruction wraps the original
// transcript; all other options retain their values. This is not a Hark ACL.
func nativeProjectPayload(v chatRequest, zone, cid string) (map[string]any, error) {
	payload := make(map[string]any)
	if len(v.OriginalBody) > 0 {
		var original map[string]json.RawMessage
		if json.Unmarshal(v.OriginalBody, &original) != nil {
			return nil, &upstreamError{400, "invalid_json", "聊天请求需要有效 JSON"}
		}
		for name, value := range original {
			switch name {
			case "model", "stream", "messages", "session_id", "session_end":
				// Gateway model alias and response format; messages maps to message.
			default:
				payload[name] = value
			}
		}
	}
	payload["conversationId"] = cid
	payload["message"] = projectScopedMessage(cid, transcript(v))
	if _, provided := payload["timezone"]; !provided {
		payload["timezone"] = zone
	}
	return payload, nil
}
