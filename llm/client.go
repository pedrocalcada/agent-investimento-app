package llm

import (
	"agent-investimento/config"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const defaultTimeout = 60 * time.Second

// Message representa uma mensagem no formato aceito pela API de LLM.
// Pode ser usada para construir históricos de conversa (system, user, assistant).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Estruturas usadas apenas para o backend HTTP \"custom\" (compatível com implementação antiga).
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

// openaiClient retorna um cliente OpenAI configurado (API key e opcionalmente base URL).
func openaiClient() openai.Client {
	opts := []option.RequestOption{
		option.WithAPIKey(config.GetString("OPENAI_API_KEY")),
	}
	if baseURL := config.GetString("OPENAI_BASE_URL"); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return openai.NewClient(opts...)
}

// toSDKMessages converte []Message para o formato do SDK (ChatCompletionMessageParamUnion).
func toSDKMessages(messages []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "system":
			out = append(out, openai.SystemMessage(m.Content))
		case "user":
			out = append(out, openai.UserMessage(m.Content))
		case "assistant":
			out = append(out, openai.AssistantMessage(m.Content))
		default:
			out = append(out, openai.UserMessage(m.Content))
		}
	}
	return out
}

// callWithMessagesOpenAI usa o SDK oficial da OpenAI.
func callWithMessagesOpenAI(ctx context.Context, messages []Message) (string, error) {
	client := openaiClient()
	model := config.GetString("OPENAI_MODEL")
	if model == "" {
		model = string(shared.ChatModelGPT4o)
	}

	chatCompletion, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(model),
		Messages: toSDKMessages(messages),
	})
	if err != nil {
		return "", err
	}

	if len(chatCompletion.Choices) == 0 {
		return "", nil
	}
	content := chatCompletion.Choices[0].Message.Content
	log.Printf("LLM resposta: %s", content)
	return content, nil
}

// callWithMessagesCustom usa o backend HTTP antigo configurado via LLM_API_URL.
func callWithMessagesCustom(ctx context.Context, messages []Message) (string, error) {
	body := request{
		Model:    config.GetString("LLM_MODEL"),
		Messages: messages,
		Think:    false,
		Stream:   false,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.GetString("LLM_API_URL"), bytes.NewReader(jsonBody))
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

// CallWithMessages envia uma lista de mensagens (incluindo system, histórico e usuário)
// para o provedor configurado (OpenAI SDK ou backend HTTP custom) e retorna o conteúdo da resposta.
func CallWithMessages(ctx context.Context, messages []Message) (string, error) {
	switch config.LLMProvider() {
	case "openai":
		return callWithMessagesOpenAI(ctx, messages)
	case "custom":
		fallthrough
	default:
		return callWithMessagesCustom(ctx, messages)
	}
}

// Call envia system e user para a API do modelo e retorna o conteúdo da resposta (message.content).
// Mantida para compatibilidade; internamente delega para CallWithMessages.
func Call(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMessage},
	}
	return CallWithMessages(ctx, messages)
}
