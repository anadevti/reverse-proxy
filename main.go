package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Estrutura para armazenar dados em cache com controle de TTL (tempo de vida)
type Cache struct {
	data map[string][]byte    // Armazena os dados em cache
	ttl  map[string]time.Time // Armazena os tempos de expiração dos dados
	mu   sync.RWMutex         // Mutex para sincronizar o acesso ao cache
}

// Estrutura do proxy reverso, com rotas e cache
type ReverseProxy struct {
	routes map[string][]string // Map de rotas para backends
	cache  Cache               // Instância do cache
}

// Construtor para a estrutura Cache
func NewCache() *Cache {
	return &Cache{
		data: make(map[string][]byte),
		ttl:  make(map[string]time.Time),
	}
}

// Construtor para a estrutura ReverseProxy
func NewReverseProxy() *ReverseProxy {
	return &ReverseProxy{
		// Configuração inicial de rotas e seus backends
		routes: map[string][]string{
			"/todos/1": {
				"https://jsonplaceholder.typicode.com",
				"https://jsonplaceholder.typicode.com",
			},
		},
		cache: *NewCache(), // Instância de cache
	}
}

// Recupera dados do cache, verificando se ainda são válidos (TTL)
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock() // Bloqueio de leitura
	defer c.mu.RUnlock()

	if expiration, exist := c.ttl[key]; exist && time.Now().Before(expiration) {
		return c.data[key], true // Retorna os dados se ainda não expiraram
	}
	return nil, false
}

// Adiciona dados ao cache com um TTL
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock() // Bloqueio de escrita
	defer c.mu.Unlock()

	c.data[key] = value
	c.ttl[key] = time.Now().Add(ttl) // Calcula a data de expiração
}

// Remove entradas expiradas do cache
func (c *Cache) CleanUp() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, expiration := range c.ttl {
		if time.Now().After(expiration) { // Verifica se o TTL expirou
			delete(c.data, key)
			delete(c.ttl, key)
		}
	}
}

// Middleware para verificar e armazenar respostas no cache
func (rp *ReverseProxy) cacheMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := fmt.Sprintf("%s-%x", r.URL.Path, sha256.Sum256([]byte(r.URL.RawQuery)))
		// Tenta recuperar do cache
		if cache, ok := rp.cache.Get(key); ok {
			w.Write(cache)
			fmt.Printf("Cache hit: %s\n", r.URL.Path)
			return
		}

		// Caso não esteja no cache, cria um gravador de resposta
		recorder := &responseRecorder{
			ResponseWriter: w,
			body:           bytes.NewBuffer(nil),
		}
		next(recorder, r) // Encaminha a requisição ao handler
		// Armazena a resposta no cache
		rp.cache.Set(key, recorder.body.Bytes(), 5*time.Second)
	}
}

// Estrutura para gravar respostas enquanto as transmite
type responseRecorder struct {
	http.ResponseWriter
	body *bytes.Buffer
}

// Sobrescreve o método Write para armazenar o corpo da resposta
func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// Seleciona um backend aleatório para uma rota
func (rp *ReverseProxy) selectBackend(route string) (string, bool) {
	backends, exists := rp.routes[route]
	if !exists || len(backends) == 0 {
		return "", false
	}
	return backends[rand.Intn(len(backends))], true
}

// Transforma o corpo da resposta, substituindo "userId" por "user_id"
func transformResponse(body []byte) []byte {
	return bytes.ReplaceAll(body, []byte("userId"), []byte("user_id"))
}

// Handler principal do proxy reverso
func (rp *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Seleciona o backend apropriado
	backend, ok := rp.selectBackend(r.URL.Path)
	if !ok {
		http.Error(w, "No backend found", http.StatusBadGateway)
		return
	}

	// Valida e cria a URL do backend
	targetURL, err := url.Parse(backend)
	if err != nil {
		http.Error(w, "Invalid backend URL", http.StatusInternalServerError)
		return
	}

	// Cria a requisição para o backend
	proxyReq, err := http.NewRequest(r.Method, targetURL.String()+r.URL.Path, r.Body)
	if err != nil {
		http.Error(w, "Error creating proxy request", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header

	start := time.Now()                          // Inicia a medição de tempo
	resp, err := http.DefaultClient.Do(proxyReq) // Envia a requisição ao backend
	if err != nil {
		http.Error(w, "Error forwarding request", http.StatusBadGateway)
		log.Printf("Error forwarding to backend: %v", err)
		return
	}
	defer resp.Body.Close()

	// Lê e transforma o corpo da resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Error reading response body", http.StatusInternalServerError)
		return
	}
	body = transformResponse(body)

	// Transfere os cabeçalhos e a resposta para o cliente
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)

	// Loga a requisição
	log.Printf("Request: %s, Backend: %s, Duration: %s", r.URL.Path, backend, time.Since(start))
}

// Função principal
func main() {
	rand.Seed(time.Now().UnixNano()) // Semente para aleatoriedade
	proxy := NewReverseProxy()       // Cria o proxy reverso

	http.HandleFunc("/", proxy.cacheMiddleware(proxy.ServeHTTP)) // Configura o middleware

	log.Fatal(http.ListenAndServe(":8080", nil)) // Inicia o servidor HTTP
}
