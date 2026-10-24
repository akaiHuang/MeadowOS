package serve

import "github.com/hey-meadow/meadow-code/go/core/session"

func (sess *serveSession) openBoundChatWriter(chatID string) (*session.ChatWriter, error) {
	chat, err := session.OpenChatWriter(sess.cwd, chatID)
	if err != nil {
		return nil, err
	}
	if err := chat.BindConnectionID(sess.connectionID); err != nil {
		_ = chat.Close()
		return nil, err
	}
	if sess.connectionID != "" || sess.engine.ModelID != "" {
		if err := chat.ModelBinding(sess.engine.ModelID); err != nil {
			_ = chat.Close()
			return nil, err
		}
	}
	return chat, nil
}
