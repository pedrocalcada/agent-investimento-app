package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// LoadConfig carrega config.yaml (ou variáveis de ambiente). Para o agente, exige apenas LLM_API_URL e LLM_MODEL.
func LoadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.AutomaticEnv() // permite override por variáveis de ambiente (ex.: LLM_API_URL, LLM_MODEL, PORT)

	v.SetDefault("LLM_MODEL", "deepseek-r1:14b")
	v.SetDefault("PORT", 9001)

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("./config")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Arquivo opcional; usa defaults e env
			v.SetConfigFile("")
		} else {
			return nil, fmt.Errorf("falha ao ler config: %w", err)
		}
	}

	if v.GetString("LLM_API_URL") == "" {
		return nil, fmt.Errorf("defina LLM_API_URL no config.yaml ou variável de ambiente")
	}

	return v, nil
}
