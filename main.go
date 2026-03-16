package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"agent-investimento/config"
	"agent-investimento/llm"
	"agent-investimento/redisstore"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
)

// investmentExecutor implementa a lógica do agente de investimentos.
// Ele segue o protocolo A2A e responde como "agente-investimento".
type investmentExecutor struct {
	mu       sync.Mutex
	balances map[string]float64       // chave: "poupanca" ou "cdb"
	sessions map[string][]llm.Message // histórico de conversa por ContextID
}

var _ a2asrv.AgentExecutor = (*investmentExecutor)(nil)

func newInvestmentExecutor() *investmentExecutor {
	return &investmentExecutor{
		balances: map[string]float64{
			"poupanca": 100000,
			"cdb":      100000,
		},
		sessions: make(map[string][]llm.Message),
	}
}

// Execute é chamado pelo servidor A2A sempre que chega uma mensagem para o agente.
// Responde imediatamente com Task em estado Working (Final); processa e envia o resultado para o callback_url.
func (e *investmentExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	msg := reqCtx.Message

	callbackURL := getCallbackURLFromMessage(msg)

	if err := e.writeStatusFinal(ctx, reqCtx, q, a2a.TaskStateWorking); err != nil {
		return err
	}

	ctxWork := context.WithoutCancel(ctx)
	go func() {

		text, state, err := e.computeResponse(ctxWork, reqCtx.ContextID, msg)
		if err != nil {
			log.Printf("falha ao computar resposta: %v", err)
			text = err.Error()
			state = a2a.TaskStateFailed
		}
		task := e.buildTaskFromResponse(reqCtx, text, state)

		postTaskToCallback(ctxWork, callbackURL, task)

	}()
	return nil
}

func (e *investmentExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	// Para este agente simples, não há trabalho assíncrono longo; nada especial a cancelar.
	return nil
}

// analyzeIntent usa o mesmo cliente LLM do projeto para extrair produto, operação e valor.
// Quando produto for indefinido, o LLM pode retornar mensagem_para_usuario para desambiguar.
// Espera-se um JSON no formato:
// {"produto":"poupanca|cdb|indefinido","operacao":"saldo|depositar|resgatar","valor":123.45,"mensagem_para_usuario":"..." (quando produto indefinido)}
type investmentIntent struct {
	Product             string  `json:"produto"`
	Operation           string  `json:"operacao"`
	Amount              float64 `json:"valor"`
	MensagemParaUsuario string  `json:"mensagem_para_usuario"`
}

const investmentSystemPrompt = `
Você é um analisador de pedidos de investimentos em uma conversa com o usuário.
Receberá mensagens em português sobre CDB ou poupança.

Você deve desambiguar entre os produtos CDB e poupança quando não ficar claro.
- produto: "poupanca", "cdb" ou "indefinido" (use "indefinido" quando a mensagem não permitir saber se é CDB ou poupança)
- operacao: "saldo", "depositar" ou "resgatar"
- valor: número em reais (use 0 para saldo ou quando não houver valor claro)
- mensagem_para_usuario: quando produto for "indefinido", este campo é OBRIGATÓRIO. Escreva uma pergunta curta e natural para o usuário escolher entre CDB e poupança, como numa conversa (ex: "Você gostaria de ver o saldo na poupança ou no CDB?", "Quer depositar na poupança ou no CDB?"). Não use texto genérico; adapte à operação que o usuário pediu.

Responda SOMENTE com um JSON neste formato:
{"produto":"poupanca|cdb|indefinido","operacao":"saldo|depositar|resgatar","valor":123.45,"mensagem_para_usuario":"sua pergunta aqui quando produto for indefinido"}
`

func (e *investmentExecutor) analyzeIntent(ctx context.Context, sessionID, userText string) (investmentIntent, error) {
	// Monta o histórico de conversa para o LLM: system + histórico + última mensagem do usuário.
	history := e.getSessionHistory(sessionID)
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{Role: "system", Content: investmentSystemPrompt})
	messages = append(messages, history...)

	content, err := llm.CallWithMessages(ctx, messages)
	if err != nil {
		return investmentIntent{}, err
	}
	content = strings.TrimSpace(content)
	// Tentar isolar o JSON (caso o modelo envolva em texto extra).
	if i := strings.Index(content, "{"); i >= 0 {
		content = content[i:]
	}
	if i := strings.LastIndex(content, "}"); i >= 0 {
		content = content[:i+1]
	}
	var intent investmentIntent
	if err := json.Unmarshal([]byte(content), &intent); err != nil {
		return investmentIntent{}, fmt.Errorf("falha ao parsear JSON do LLM: %w", err)
	}

	// Normalização básica.
	intent.Product = strings.ToLower(strings.TrimSpace(intent.Product))
	intent.Operation = strings.ToLower(strings.TrimSpace(intent.Operation))
	if intent.Product == "indefinido" {
		intent.Product = ""
	}
	return intent, nil
}

const disambiguationPrompt = `
Você está em uma conversa com o usuário sobre investimentos (CDB e poupança).
O usuário disse algo que não deixa claro se quer operar em CDB ou poupança.
Gere UMA pergunta curta e natural, em português, para o usuário escolher entre CDB e poupança.
Responda SOMENTE com a pergunta, sem aspas nem texto adicional.
`

func (e *investmentExecutor) askDisambiguationFromLLM(ctx context.Context, sessionID, userText string) (string, error) {
	history := e.getSessionHistory(sessionID)
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{Role: "system", Content: disambiguationPrompt})
	messages = append(messages, history...)

	msg, err := llm.CallWithMessages(ctx, messages)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(msg), nil
}

// responseSatisfiesIntent pergunta ao LLM se a resposta proposta satisfaz a intenção original do usuário
// considerando todo o histórico da conversa. Retorna true apenas quando o LLM responde "satisfaz".
func (e *investmentExecutor) responseSatisfiesIntent(ctx context.Context, sessionID, proposedResponse string) (bool, error) {
	history := e.getSessionHistory(sessionID)
	messages := make([]llm.Message, 0, len(history)+2)
	messages = append(messages, llm.Message{Role: "system", Content: satisfactionCheckPrompt})
	messages = append(messages, history...)
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: fmt.Sprintf("A resposta que o agente vai dar ao usuário é:\n\n\"%s\"\n\nConsiderando todo o histórico da conversa acima, essa resposta satisfaz completamente a intenção original do usuário? Responda SOMENTE com uma das palavras: satisfaz ou nao_satisfaz.", proposedResponse),
	})

	content, err := llm.CallWithMessages(ctx, messages)
	if err != nil {
		return false, err
	}
	content = strings.ToLower(strings.TrimSpace(content))
	if strings.Contains(content, "nao_satisfaz") {
		return false, nil
	}
	return strings.Contains(content, "satisfaz"), nil
}

const satisfactionCheckPrompt = `
Você é um validador de conversas. Receberá o histórico de uma conversa entre usuário e um agente de investimentos (CDB/poupança: saldo, depositar, resgatar) e a resposta que o agente pretende dar ao usuário.

Sua única tarefa: decidir se essa resposta SATISFAZ COMPLETAMENTE a intenção original do usuário na conversa.

- Se o usuário pediu para resgatar e o agente só informou saldo (porque o usuário perguntou o saldo no meio), a intenção original NÃO foi satisfeita → nao_satisfaz.
- Se o usuário pediu algo e o agente fez exatamente isso (ou perguntou onde fazer e ainda está esperando), considere o contexto: resposta que encerra a tarefa pedida = satisfaz; resposta que só ajuda mas não conclui a tarefa = nao_satisfaz.

Responda SOMENTE com uma das duas palavras: satisfaz ou nao_satisfaz.
`

// writeStatusFinal escreve apenas um TaskStatusUpdateEvent (Final) com o estado dado; sem mensagem.
func (e *investmentExecutor) writeStatusFinal(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue, state a2a.TaskState) error {
	status := a2a.NewStatusUpdateEvent(reqCtx, state, nil)
	status.Final = true
	if err := q.Write(ctx, status); err != nil {
		return fmt.Errorf("falha ao escrever status final: %w", err)
	}
	return nil
}

// computeResponse executa a lógica de negócio e retorna texto, estado final e eventual erro.
func (e *investmentExecutor) computeResponse(ctx context.Context, sessionID string, msg *a2a.Message) (text string, state a2a.TaskState, err error) {
	userText := extractTextFromMessage(msg)
	if strings.TrimSpace(userText) == "" {
		userText = "mensagem vazia"
	}

	// Primeiro registramos a mensagem do usuário no histórico da sessão.
	e.appendSessionMessages(sessionID, llm.Message{
		Role:    "user",
		Content: userText,
	})

	intent, err := e.analyzeIntent(ctx, sessionID, userText)
	if err != nil {
		return fmt.Sprintf("não consegui entender sua solicitação de investimento: %v", err), a2a.TaskStateFailed, err
	}

	if intent.Product == "" {
		resp := strings.TrimSpace(intent.MensagemParaUsuario)
		if resp == "" {
			resp, err = e.askDisambiguationFromLLM(ctx, sessionID, userText)
			if err != nil {
				return fmt.Sprintf("não consegui formular a pergunta: %v", err), a2a.TaskStateFailed, err
			}
		}
		// Registramos a pergunta de desambiguação como resposta do agente.
		e.appendSessionMessages(sessionID, llm.Message{
			Role:    "assistant",
			Content: resp,
		})
		return resp, a2a.TaskStateInputRequired, nil
	}

	switch intent.Operation {
	case "saldo":
		saldo := e.getBalance(intent.Product)
		resp := fmt.Sprintf("Seu saldo em %s é de R$ %.2f.", intent.Product, saldo)
		satisfaz, errS := e.responseSatisfiesIntent(ctx, sessionID, resp)
		if errS != nil {
			satisfaz = true
		}
		e.appendSessionMessages(sessionID, llm.Message{
			Role:    "assistant",
			Content: resp,
		})
		if !satisfaz {
			resp += " Para concluir sua solicitação anterior, diga se é na poupança ou no CDB."
			return resp, a2a.TaskStateInputRequired, nil
		}
		return resp, a2a.TaskStateCompleted, nil
	case "depositar":
		saldo := e.changeBalance(intent.Product, intent.Amount)
		resp := fmt.Sprintf("Depositei R$ %.2f em %s. Saldo atual: R$ %.2f.", intent.Amount, intent.Product, saldo)
		satisfaz, errS := e.responseSatisfiesIntent(ctx, sessionID, resp)
		if errS != nil {
			satisfaz = true
		}
		e.appendSessionMessages(sessionID, llm.Message{
			Role:    "assistant",
			Content: resp,
		})
		if !satisfaz {
			resp += " Para concluir sua solicitação anterior, diga se é na poupança ou no CDB."
			return resp, a2a.TaskStateInputRequired, nil
		}
		return resp, a2a.TaskStateCompleted, nil
	case "resgatar":
		saldo := e.changeBalance(intent.Product, -intent.Amount)
		resp := fmt.Sprintf("Resgatei R$ %.2f de %s. Saldo atual: R$ %.2f.", intent.Amount, intent.Product, saldo)
		satisfaz, errS := e.responseSatisfiesIntent(ctx, sessionID, resp)
		if errS != nil {
			satisfaz = true
		}
		e.appendSessionMessages(sessionID, llm.Message{
			Role:    "assistant",
			Content: resp,
		})
		if !satisfaz {
			resp += " Para concluir sua solicitação anterior, diga se é na poupança ou no CDB."
			return resp, a2a.TaskStateInputRequired, nil
		}
		return resp, a2a.TaskStateCompleted, nil
	default:
		resp := "Não entendi se você quer consultar saldo, depositar ou resgatar em CDB ou poupança."
		satisfaz, errS := e.responseSatisfiesIntent(ctx, sessionID, resp)
		if errS != nil {
			satisfaz = true
		}
		e.appendSessionMessages(sessionID, llm.Message{
			Role:    "assistant",
			Content: resp,
		})
		if !satisfaz {
			resp += " Para concluir sua solicitação anterior, diga se é na poupança ou no CDB."
			return resp, a2a.TaskStateInputRequired, nil
		}
		return resp, a2a.TaskStateCompleted, nil
	}
}

// buildTaskFromResponse monta o Task final para enviar ao callback (Completed, InputRequired ou Failed).
// Se reqCtx.StoredTask estiver preenchido (task recuperada do Redis), usa essa task como base em vez de criar uma nova.
func (e *investmentExecutor) buildTaskFromResponse(reqCtx *a2asrv.RequestContext, text string, state a2a.TaskState) *a2a.Task {
	cmd := redisstore.Client().Get(context.Background(), string(reqCtx.StoredTask.ID))
	if err := cmd.Err(); err != nil {
		log.Printf("falha ao recuperar task do Redis: %v", err)
		return nil
	}
	var task a2a.Task
	if err := json.Unmarshal([]byte(cmd.Val()), &task); err != nil {
		log.Printf("falha ao deserializar task do Redis: %v", err)
		return nil
	}
	task.Status.State = state
	task.History = append(task.History, a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: text}))
	return &task
}

func getCallbackURLFromMessage(m *a2a.Message) string {
	s, _ := m.Metadata["callback_url"].(string)
	return strings.TrimSpace(s)
}

func (e *investmentExecutor) getBalance(product string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.balances[product]
}

func (e *investmentExecutor) changeBalance(product string, delta float64) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.balances[product] += delta
	return e.balances[product]
}

// getSessionHistory retorna uma cópia do histórico de mensagens da sessão.
func (e *investmentExecutor) getSessionHistory(sessionID string) []llm.Message {
	if sessionID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.sessions[sessionID]
	if len(h) == 0 {
		return nil
	}
	out := make([]llm.Message, len(h))
	copy(out, h)
	return out
}

// appendSessionMessages adiciona mensagens ao histórico da sessão.
func (e *investmentExecutor) appendSessionMessages(sessionID string, msgs ...llm.Message) {
	if sessionID == "" || len(msgs) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string][]llm.Message)
	}
	e.sessions[sessionID] = append(e.sessions[sessionID], msgs...)
}

func extractTextFromMessage(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var out []string
	for _, p := range m.Parts {
		if t, ok := p.(a2a.TextPart); ok {
			out = append(out, t.Text)
		}
	}
	return strings.Join(out, "\n")
}

var httpPort = flag.Int("port", 9001, "Porta para o servidor A2A JSON-RPC do agente de investimentos.")

// agentCardFileHandler retorna um handler que serve o JSON do agent-card, substituindo __PORT__ pela porta real.
func agentCardFileHandler(jsonPath string, port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			log.Printf("agent-card: falha ao ler %s: %v", jsonPath, err)
			http.Error(w, "agent card indisponível", http.StatusInternalServerError)
			return
		}
		body := bytes.ReplaceAll(data, []byte("__PORT__"), []byte(fmt.Sprintf("%d", port)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func postTaskToCallback(ctx context.Context, callbackURL string, task *a2a.Task) {
	data, err := json.Marshal(task)
	if err != nil {
		log.Printf("callback: falha ao serializar task: %v", err)
		return
	}
	// Usar contexto independente: o ctx do Execute() é cancelado quando o servidor A2A
	// considera a resposta "Working" (Final) como fim do request; o callback deve ser
	// enviado ao orquestrador mesmo após isso.
	reqCtx := context.WithoutCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, callbackURL, bytes.NewReader(data))
	if err != nil {
		log.Printf("callback: falha ao criar request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("callback: falha ao enviar para %s: %v", callbackURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("callback: %s retornou status %d", callbackURL, resp.StatusCode)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()

	v, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("config inválida: %v", err)
	}

	port := v.GetInt("PORT")
	if *httpPort != 9001 {
		port = *httpPort
	}

	exec := newInvestmentExecutor()

	var handlerOpts []a2asrv.RequestHandlerOption
	redisstore.Init()
	if rdb := redisstore.Client(); rdb != nil {
		handlerOpts = append(handlerOpts, a2asrv.WithTaskStore(redisstore.NewStore(rdb)))
		log.Printf("TaskStore: Redis em %s", config.RedisAddr())
	}

	agentCardPath := "agent-card.json"
	if p := v.GetString("AGENT_CARD_PATH"); p != "" {
		agentCardPath = p
	}

	requestHandler := a2asrv.NewHandler(exec, handlerOpts...)
	jsonrpcHandler := a2asrv.NewJSONRPCHandler(requestHandler)
	mux := http.NewServeMux()
	mux.Handle("/invoke", jsonrpcHandler)
	mux.Handle(a2asrv.WellKnownAgentCardPath, agentCardFileHandler(agentCardPath, port))

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("falha ao fazer listen na porta %d: %v", port, err)
	}
	log.Printf("Agente de investimentos ouvindo em http://127.0.0.1:%d (AgentCard em /.well-known/agent-card.json)", port)

	if err := http.Serve(listener, mux); err != nil {
		log.Printf("servidor do agente de investimentos finalizado: %v", err)
	}

	_ = ctx
}
