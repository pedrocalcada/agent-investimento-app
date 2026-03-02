package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

const defaultTimeout = 60 * time.Second

// Message representa uma mensagem no formato aceito pela API de LLM.
// Pode ser usada para construir históricos de conversa (system, user, assistant).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Think    bool      `json:"think"`
	Stream   bool      `json:"stream"`
}

type response struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

// CallWithMessages envia uma lista de mensagens (incluindo system, histórico e usuário)
// para a API do modelo e retorna o conteúdo da resposta (message.content).
func CallWithMessages(ctx context.Context, apiURL, model string, messages []Message) (string, error) {
	body := request{
		Model:    model,
		Messages: messages,
		Think:    false,
		Stream:   false,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: defaultTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM API status %d", resp.StatusCode)
	}

	var apiResp response
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", err
	}
	log.Printf("LLM resposta: %s", apiResp.Message.Content)
	return apiResp.Message.Content, nil
}

// Call envia system e user para a API do modelo e retorna o conteúdo da resposta (message.content).
// Mantida para compatibilidade; internamente delega para CallWithMessages.
func Call(ctx context.Context, apiURL, model, systemPrompt, userMessage string) (string, error) {
	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMessage},
	}
	return CallWithMessages(ctx, apiURL, model, messages)
}
