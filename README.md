# Agente de Investimentos (A2A)

Agente A2A isolado para operações em CDB e poupança. Expõe JSON-RPC em `/invoke` e Agent Card em `/.well-known/agent-card.json`.

## Requisitos

- Go 1.24+
- API de LLM compatível (modelo, system/user messages, JSON de resposta)

## Configuração

- `config.yaml` na raiz (ou use `config.yaml.example` como base), ou variáveis de ambiente:
  - **LLM_API_URL** (obrigatório): URL da API do modelo (ex.: `http://localhost:11434/v1/chat/completions`)
  - **LLM_MODEL**: modelo a usar (default: `deepseek-r1:14b`)
  - **PORT**: porta HTTP do agente (default: 9001)

## Uso

```bash
# Com config.yaml
go run .

# Override por env
LLM_API_URL=http://localhost:11434/v1/chat/completions PORT=9001 go run .

# Flag -port
go run . -port 9002
```

## Orquestrador

O orquestrador envia tarefas com `callback_url` nos metadados; o agente responde com Working (Final) e depois envia o resultado (Task Completed/InputRequired/Failed) para essa URL (POST JSON).

## Projeto independente

Este diretório é autocontido. Para usar como repositório separado:

1. Copie a pasta `agent-investimento-app` para o novo repositório.
2. Ajuste o `module` em `go.mod` se quiser outro nome (ex.: `github.com/seu-org/agent-investimento`).
3. Atualize os imports em `main.go` (config e llm) para o novo caminho do módulo.
