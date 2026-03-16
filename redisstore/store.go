package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"

	"agent-investimento/config"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/redis/go-redis/v9"
)

const (
	keySetAllTasks = "a2a:tasks:all"
)

var (
	globalClient *redis.Client
	initOnce     sync.Once
)

// ErrTaskAlreadyExists é retornado por Save quando a task já existe (create com prev = missing).
var ErrTaskAlreadyExists = errors.New("task already exists")

// Init inicializa o cliente Redis global (uma única vez), lendo addr, senha e DB do config.
// Se REDIS_ADDR estiver vazio, o cliente não é criado e Client() retornará nil.
// Se o Redis estiver configurado mas indisponível, a aplicação encerra com log.Fatal.
func Init() {
	initOnce.Do(func() {
		addr := config.RedisAddr()
		if addr == "" {
			return
		}
		globalClient = redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: config.RedisPassword(),
			DB:       config.RedisDB(),
		})
		if err := globalClient.Ping(context.Background()).Err(); err != nil {
			log.Fatalf("Redis indisponível em %s: %v", addr, err)
		}
	})
}

// Client retorna o cliente Redis global. Só deve ser chamado após Init.
func Client() *redis.Client {
	return globalClient
}

// Store implementa a2asrv.TaskStore usando Redis.
// Todas as instâncias do agente devem usar o mesmo Redis para compartilhar tasks.
type Store struct {
	rdb *redis.Client
}

// NewStore cria um TaskStore que persiste em Redis.
// rdb deve ser compartilhado entre todas as instâncias do agente.
// Para usar o cliente global, passe redisstore.Client().
func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func taskKey(id a2a.TaskID) string {
	return string(id)
}

// Save implementa a2asrv.TaskStore.
// Se prev == a2a.TaskVersionMissing (ou 0), cria a task (falha se já existir).
// Caso contrário, faz um update simples, sem controle de concorrência.
func (s *Store) Save(ctx context.Context, task *a2a.Task, _ a2a.Event, prev a2a.TaskVersion) (a2a.TaskVersion, error) {
	if task == nil || task.ID == "" {
		return 0, fmt.Errorf("task inválida: %w", a2a.ErrInvalidParams)
	}
	key := taskKey(task.ID)

	data, err := json.Marshal(task)
	if err != nil {
		return 0, err
	}

	_, err = s.rdb.Set(ctx, key, data, 0).Result()
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Get implementa a2asrv.TaskStore.
func (s *Store) Get(ctx context.Context, id a2a.TaskID) (*a2a.Task, a2a.TaskVersion, error) {
	task, err := s.getTask(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	// Sem controle de versão explícito, retornamos 0.
	return task, 0, nil
}

func (s *Store) getTask(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	if id == "" {
		return nil, a2a.ErrTaskNotFound
	}
	key := taskKey(id)
	cmd := s.rdb.Get(ctx, key)

	if err := cmd.Err(); err != nil {
		return nil, err
	}
	raw := cmd.Val()

	var t a2a.Task
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// List implementa a2asrv.TaskStore: filtra por ContextID, Status, LastUpdatedAfter e pagina.
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	const defaultPageSize = 50
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page_size deve estar entre 1 e 100: %w", a2a.ErrInvalidRequest)
	}

	ids, err := s.rdb.SMembers(ctx, keySetAllTasks).Result()
	if err != nil {
		return nil, err
	}

	type taskWithMeta struct {
		task *a2a.Task
	}
	var filtered []taskWithMeta
	for _, id := range ids {
		task, err := s.getTask(ctx, a2a.TaskID(id))
		if err != nil {
			if errors.Is(err, a2a.ErrTaskNotFound) {
				continue
			}
			return nil, err
		}
		if req.ContextID != "" && task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && task.Status.State != req.Status {
			continue
		}
		// Sem campo de timestamp explícito no armazenamento, ignoramos LastUpdatedAfter.
		filtered = append(filtered, taskWithMeta{task: task})
	}

	// Ordenar apenas por ID (desc) para ter ordem determinística.
	for i := 0; i < len(filtered); i++ {
		for j := i + 1; j < len(filtered); j++ {
			if filtered[j].task.ID > filtered[i].task.ID {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}

	totalSize := len(filtered)
	start := 0
	if req.PageToken != "" {
		if off, err := strconv.Atoi(req.PageToken); err == nil && off > 0 && off < len(filtered) {
			start = off
		}
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	tasks := make([]*a2a.Task, 0, len(page))
	for _, p := range page {
		task := p.task
		if req.HistoryLength > 0 && len(task.History) > req.HistoryLength {
			task = copyTaskTrimHistory(task, req.HistoryLength)
		} else if req.HistoryLength == 0 {
			task = copyTaskTrimHistory(task, 0)
		}
		if !req.IncludeArtifacts {
			task = copyTaskNoArtifacts(task)
		}
		tasks = append(tasks, task)
	}

	var nextPageToken string
	if end < totalSize {
		nextPageToken = strconv.Itoa(end)
	}

	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     totalSize,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

func copyTaskTrimHistory(t *a2a.Task, historyLength int) *a2a.Task {
	out := *t
	out.History = nil
	if historyLength == 0 {
		return &out
	}
	if len(t.History) <= historyLength {
		out.History = make([]*a2a.Message, len(t.History))
		copy(out.History, t.History)
		return &out
	}
	out.History = make([]*a2a.Message, historyLength)
	copy(out.History, t.History[len(t.History)-historyLength:])
	return &out
}

func copyTaskNoArtifacts(t *a2a.Task) *a2a.Task {
	out := *t
	out.Artifacts = nil
	return &out
}
