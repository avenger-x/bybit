package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// V5WebsocketTradeServiceI :
type V5WebsocketTradeServiceI interface {
	Start(context.Context, ErrHandler) error
	Login() error
	Run() error
	Ping() error
	Close() error
	Subscribe(func(resp *V5WebsocketTradeCreateOrderResponse)) error
	CreateOrder(reqId string, orders []*V5CreateOrderParam) error
	CancelOrder(reqId string, orders []*V5CancelOrderParam) error
}

// V5WebsocketTradeService :
type V5WebsocketTradeService struct {
	client     *WebSocketClient
	connection *websocket.Conn
	mu         sync.Mutex
	ch         chan *V5WebsocketTradeCreateOrderResponse
	chMu       sync.Mutex
}

const (
	// V5WebsocketTradePath :
	V5WebsocketTradePath = "/v5/trade"
)

// V5WebsocketTradeTopic :
type V5WebsocketTradeTopic string

const (
	// V5WebsocketTradeTopicPong :
	V5WebsocketTradeTopicPong        V5WebsocketTradeTopic = "pong"
	V5WebsocketTradeTopicOrderCreate                       = "order.create"
)

type V5WebsocketTradeCreateOrderResponse struct {
	ReqId   string                 `json:"reqId"`
	RetCode int                    `json:"retCode"`
	RetMsg  string                 `json:"retMsg"`
	Op      string                 `json:"op"`
	Data    map[string]interface{} `json:"data"`
}

// judgeTopic :
func (s *V5WebsocketTradeService) judgeTopic(respBody []byte) (V5WebsocketTradeTopic, error) {
	parsedData := map[string]interface{}{}
	if err := json.Unmarshal(respBody, &parsedData); err != nil {
		return "", err
	}
	if retMsg, ok := parsedData["op"].(string); ok {
		switch retMsg {
		case "pong":
			return V5WebsocketTradeTopicPong, nil
		case "order.create":
			return V5WebsocketTradeTopicOrderCreate, nil
		}
	}

	if authStatus, ok := parsedData["success"].(bool); ok {
		if !authStatus {
			return "", errors.New("auth failed: " + parsedData["ret_msg"].(string))
		}
	}
	return "", nil
}

// Login : Apply for authentication when establishing a connection.
func (s *V5WebsocketTradeService) Login() error {
	param, err := s.client.buildAuthParam()
	if err != nil {
		return err
	}
	if err := s.writeMessage(websocket.TextMessage, param); err != nil {
		return err
	}
	return nil
}

// Start :
func (s *V5WebsocketTradeService) Start(ctx context.Context, errHandler ErrHandler) error {
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer s.connection.Close()

		_ = s.connection.SetReadDeadline(time.Now().Add(60 * time.Second))
		s.connection.SetPongHandler(func(string) error {
			_ = s.connection.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})

		for {
			if err := s.Run(); err != nil {
				if errHandler == nil {
					return
				}
				errHandler(IsErrWebsocketClosed(err), err)
				return
			}
		}
	}()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	for {
		select {
		case <-done:
			return nil
		case <-ticker.C:
			if err := s.Ping(); err != nil {
				return err
			}
		case <-ctx.Done():
			s.client.debugf("caught websocket trade service interrupt signal")

			if err := s.Close(); err != nil {
				return err
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return nil
		}
	}
}
func (s *V5WebsocketTradeService) Subscribe(f func(resp *V5WebsocketTradeCreateOrderResponse)) error {
	s.chMu.Lock()
	ch := s.ch
	s.chMu.Unlock()
	if ch != nil {
		return fmt.Errorf("s.ch != nil")
	}
	ch = make(chan *V5WebsocketTradeCreateOrderResponse)
	s.chMu.Lock()
	s.ch = ch
	s.chMu.Unlock()
	for {
		select {
		case resp := <-ch:
			go f(resp)
		}
	}
}

func (s *V5WebsocketTradeService) UnSubscribe() {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	s.ch = nil
}

// Run :
func (s *V5WebsocketTradeService) Run() error {
	_, message, err := s.connection.ReadMessage()
	if err != nil {
		return err
	}

	topic, err := s.judgeTopic(message)
	if err != nil {
		return err
	}
	switch topic {
	case V5WebsocketTradeTopicPong:
		if err := s.connection.PongHandler()("pong"); err != nil {
			return fmt.Errorf("pong: %w", err)
		}
	case V5WebsocketTradeTopicOrderCreate:
		res := &V5WebsocketTradeCreateOrderResponse{}
		err = json.Unmarshal(message, res)
		if err != nil {
			return fmt.Errorf("json.Unmarshal err: %w", err)
		}
		s.chMu.Lock()
		ch := s.ch
		s.chMu.Unlock()
		if ch != nil {
			go func() {
				ch <- res
			}()
		}
	}
	return nil
}

// Ping :
func (s *V5WebsocketTradeService) Ping() error {
	// NOTE: It appears that two messages need to be sent.
	// REF: https://github.com/hirokisan/bybit/pull/127#issuecomment-1537479346
	if err := s.writeControl(websocket.PingMessage, nil); err != nil {
		return err
	}
	if err := s.writeMessage(websocket.TextMessage, []byte(`{"op":"ping"}`)); err != nil {
		return err
	}
	return nil
}

// Close :
func (s *V5WebsocketTradeService) Close() error {
	if err := s.writeControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil && !errors.Is(err, websocket.ErrCloseSent) {
		return err
	}
	return nil
}

func (s *V5WebsocketTradeService) writeMessage(messageType int, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_ = s.connection.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if err := s.connection.WriteMessage(messageType, body); err != nil {
		return err
	}
	return nil
}

func (s *V5WebsocketTradeService) writeControl(messageType int, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.connection.WriteControl(messageType, body, time.Now().Add(60*time.Second)); err != nil {
		return err
	}
	return nil
}
