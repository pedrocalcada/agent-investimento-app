package config

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// globalV guarda a instância do viper após LoadConfig(); usado por GetString, GetStringMapString, etc.
var globalV *viper.Viper

func LoadConfig() (*viper.Viper, error) {
	v := viper.New()

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("./config")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("falha ao ler config.yaml: %w", err)
	}

	provider := v.GetString("LLM_PROVIDER")
	if provider == "" {
		provider = "custom"
	}
	switch provider {
	case "openai":
		if v.GetString("OPENAI_API_KEY") == "" {
			return nil, errors.New("defina OPENAI_API_KEY no config.yaml ou variável de ambiente (API key da OpenAI) quando LLM_PROVIDER=openai")
		}
	case "custom":
		if v.GetString("LLM_API_URL") == "" {
			return nil, errors.New("defina LLM_API_URL no config.yaml (endpoint HTTP da API do modelo) quando LLM_PROVIDER=custom")
		}
	default:
		return nil, fmt.Errorf("LLM_PROVIDER inválido: %s (use \"openai\" ou \"custom\")", provider)
	}

	globalV = v
	return v, nil
}

// GetString retorna o valor string da chave de configuração. Pode ser chamado de qualquer lugar após LoadConfig().
func GetString(key string) string {
	if globalV == nil {
		return ""
	}
	return globalV.GetString(key)
}

// GetStringMapString retorna o mapa string->string da chave. Útil para configs como agent_urls.
func GetStringMapString(key string) map[string]string {
	if globalV == nil {
		return nil
	}
	return globalV.GetStringMapString(key)
}

// AgentURLs retorna o mapa nome-do-agente -> URL base do AgentCard A2A.
// Chaves esperadas: agente-investimento, agente-pagamentos, agente-geral (conforme planner).
func AgentURLs() map[string]string {
	out := make(map[string]string)
	if m := GetStringMapString("agent_urls"); len(m) > 0 {
		for k, u := range m {
			if u != "" {
				out[k] = u
			}
		}
	}
	for _, name := range []string{"agente-investimento", "agente-pagamentos", "agente-geral"} {
		key := "AGENT_URL_" + name
		if u := GetString(key); u != "" {
			out[name] = u
		}
	}
	return out
}

// CallbackListenAddr retorna o endereço em que o orquestrador escuta callbacks dos agentes (ex.: ":8080").
// Se vazio, o servidor de callback não é iniciado.
func CallbackListenAddr() string {
	return GetString("ORCHESTRATOR_CALLBACK_LISTEN")
}

// CallbackBaseURL retorna a URL base que o orquestrador informa aos agentes para devolver a resposta (ex.: "http://localhost:8080").
func CallbackBaseURL() string {
	return GetString("ORCHESTRATOR_CALLBACK_BASE_URL")
}

// RedisAddr retorna o endereço do Redis para o SessionStore (ex.: "localhost:6379"). Se vazio, usa store em memória.
func RedisAddr() string {
	return GetString("REDIS_ADDR")
}

// RedisPassword retorna a senha do Redis (opcional).
func RedisPassword() string {
	return GetString("REDIS_PASSWORD")
}

// RedisDB retorna o número do DB Redis (0 por padrão).
func RedisDB() int {
	if v := globalV; v != nil && v.IsSet("REDIS_DB") {
		return v.GetInt("REDIS_DB")
	}
	return 0
}

// LLMProvider retorna o provedor de LLM configurado (\"openai\" ou \"custom\").
// Se não definido, assume \"custom\" para manter compatibilidade com a implementação antiga.
func LLMProvider() string {
	if globalV == nil {
		return "custom"
	}
	p := globalV.GetString("LLM_PROVIDER")
	if p == "" {
		return "custom"
	}
	return p
}
