package sessao

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/GitDavidJr/rustdesk-mcp/internal/maquinas"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/files"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/forward"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/peer"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/rendezvous"
)

// Gerente guarda as máquinas conectadas. Uma por ID, compartilhada por quem usa o MCP.
type Gerente struct {
	Servidor rendezvous.Server
	Cadastro *maquinas.Registro
	Ocioso   time.Duration
	// Nome que aparece para a pessoa na janela de conexão do RustDesk.
	Nome string

	mu     sync.Mutex
	ativas map[string]*Ativa
}

// Pedido de conexão.
type Pedido struct {
	Maquina                     string // apelido cadastrado ou ID
	Senha                       string // vazia: usa o acesso fixo cadastrado
	Cliente, Autorizado         string
	UsuarioWindows, SenhaWindow string // administrador do Windows: libera o terminal de administrador
	// ExigirCliente recusa máquina cadastrada ou já conectada em nome de outro cliente. É para quem chama em nome
	// de um atendimento e não pode alcançar a máquina de outro.
	ExigirCliente bool
	// Reconectar derruba a conexão que existe e abre outra; sem isso, quem chega usa a que está aberta.
	Reconectar bool
}

// ErrOutroCliente: a máquina pertence a outro cliente.
var ErrOutroCliente = errors.New("essa máquina é de outro cliente")

// Ativa é uma máquina conectada; terminal, tela e arquivos abrem quando usados.
type Ativa struct {
	ID, Apelido, Cliente, Autorizado string
	g                                *Gerente
	senha                            string
	hash                             []byte
	usuario, senhaWin                string
	sessao                           uint64

	cred sync.Mutex // senha, hash e Info

	mu        sync.Mutex
	term      *Terminal
	admin     *Terminal
	conversa  *Terminal
	tela      *Tela
	arq       *files.Client
	tuneis    map[string]*forward.Mapping
	admins    map[string]string // pedidos de administrador à espera: id -> base dos arquivos
	Info      *pb.PeerInfo
	ultimoUso time.Time
}

type semPergunta struct{}

func (semPergunta) Password(errada bool) (string, error) {
	if errada {
		return "", errors.New("senha recusada pela máquina (a de uso único muda depois de cada entrada)")
	}
	return "", errors.New("a máquina pede senha e não há acesso fixo cadastrado")
}
func (semPergunta) TwoFactor(bool) (string, error) {
	return "", errors.New("a máquina pede código de duas etapas")
}
func (semPergunta) Status(string) {}

// Conectar valida a senha abrindo o terminal (ou a tela, se o terminal não estiver disponível).
func (g *Gerente) Conectar(ctx context.Context, p Pedido) (*Ativa, error) {
	m, cadastrada := g.Cadastro.Achar(p.Maquina)
	id := m.ID
	if !cadastrada {
		if id = maquinas.SoNumeros(p.Maquina); id == "" {
			return nil, fmt.Errorf("máquina %q não cadastrada e não é um ID", p.Maquina)
		}
	}
	if p.ExigirCliente && cadastrada && !maquinas.MesmoCliente(m.Cliente, p.Cliente) {
		return nil, ErrOutroCliente
	}
	if aberta := g.pegar(id); aberta != nil {
		if p.ExigirCliente && !maquinas.MesmoCliente(aberta.Cliente, p.Cliente) {
			return nil, ErrOutroCliente
		}
		if !p.Reconectar && aberta.Viva() {
			aberta.usar()
			return aberta, nil
		}
	}
	if !cadastrada {
		if p.Senha == "" {
			return nil, errors.New("máquina sem acesso fixo: informe a senha que o cliente mandou")
		}
		if strings.TrimSpace(p.Cliente) == "" || strings.TrimSpace(p.Autorizado) == "" {
			return nil, errors.New("informe o cliente e quem autorizou o acesso")
		}
	}
	a := &Ativa{ID: id, Apelido: m.Apelido, Cliente: m.Cliente, Autorizado: m.Autorizado, g: g, senha: p.Senha, usuario: p.UsuarioWindows, senhaWin: p.SenhaWindow, sessao: peer.NovaSessao(), tuneis: map[string]*forward.Mapping{}, admins: map[string]string{}, ultimoUso: time.Now()}
	if p.Senha == "" {
		a.hash = m.Hash
	}
	if p.Cliente != "" {
		a.Cliente = p.Cliente
	}
	if p.Autorizado != "" {
		a.Autorizado = p.Autorizado
	}
	if velha := g.pegar(id); velha != nil {
		velha.Fechar()
	}
	_, errT := a.Terminal(ctx)
	if errT != nil {
		if pe := (*peer.Error)(nil); errors.As(errT, &pe) && (pe.Code == peer.CodeWrongPassword || pe.Code == peer.CodeCancelled) {
			return nil, errT
		}
		if _, errTela := a.Tela(ctx); errTela != nil {
			return nil, fmt.Errorf("terminal: %v; tela: %v", errT, errTela)
		}
	}
	g.mu.Lock()
	if g.ativas == nil {
		g.ativas = map[string]*Ativa{}
	}
	g.ativas[id] = a
	g.mu.Unlock()
	go a.vigiar()
	return a, nil
}

func (g *Gerente) pegar(id string) *Ativa { g.mu.Lock(); defer g.mu.Unlock(); return g.ativas[id] }

// Pegar a máquina conectada, por apelido ou ID.
func (g *Gerente) Pegar(nome string) (*Ativa, error) {
	id := maquinas.SoNumeros(nome)
	if m, ok := g.Cadastro.Achar(nome); ok {
		id = m.ID
	}
	a := g.pegar(id)
	if a == nil {
		return nil, fmt.Errorf("%s não está conectada: use conectar", nome)
	}
	a.usar()
	return a, nil
}

func (a *Ativa) usar() {
	a.mu.Lock()
	a.ultimoUso = time.Now()
	a.mu.Unlock()
}

// Viva diz se alguma conexão com a máquina está de pé.
func (a *Ativa) Viva() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range []*Terminal{a.term, a.conversa, a.admin} {
		if t != nil && t.Vivo() {
			return true
		}
	}
	return a.tela != nil && a.tela.Vivo()
}

// Conectadas agora.
func (g *Gerente) Conectadas() []*Ativa {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*Ativa, 0, len(g.ativas))
	for _, a := range g.ativas {
		out = append(out, a)
	}
	return out
}

func (a *Ativa) vigiar() {
	for {
		time.Sleep(time.Minute)
		if a.g.pegar(a.ID) != a {
			return
		}
		a.mu.Lock()
		parado := time.Since(a.ultimoUso)
		a.mu.Unlock()
		if a.g.Ocioso > 0 && parado > a.g.Ocioso {
			a.Fechar()
			return
		}
	}
}

func (a *Ativa) conectar(ctx context.Context, tipo pb.ConnType, login func(*pb.LoginRequest), codigo func(string) peer.Code) (*peer.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.cred.Lock()
	senha, hash := a.senha, a.hash
	a.cred.Unlock()
	s, err := peer.Connect(ctx, peer.Options{
		Server: a.g.Servidor, PeerID: a.ID, ConnType: tipo, Login: login, Password: senha, RememberedHash: hash,
		Prompter: semPergunta{}, MyID: "rustdesk-mcp", MyName: a.g.Nome, MyPlatform: "Linux", Version: "1.5.3",
		SessionID: a.sessao, LoginErrorCode: codigo,
	})
	if err != nil {
		return nil, err
	}
	a.cred.Lock()
	defer a.cred.Unlock()
	a.Info = s.PeerInfo
	if len(s.Hash) > 0 {
		a.hash, a.senha = s.Hash, "" // daqui em diante basta o hash; a senha sai da memória
	}
	return s, nil
}

func codigoTerminal(e string) peer.Code {
	if l := strings.ToLower(e); strings.Contains(l, "no permission") || strings.Contains(l, "not allowed") {
		return peer.CodePermissionDenied
	}
	return peer.CodeLogin
}

// Terminal do usuário logado, reaberto se caiu.
func (a *Ativa) Terminal(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.term != nil && a.term.Vivo() {
		return a.term, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal("", ""), codigoTerminal)
	if err != nil {
		return nil, err
	}
	a.term, err = AbrirTerminal(ctx, s)
	return a.term, err
}

// Conversa é o terminal interativo, separado do que roda comandos: um sqlcmd aberto não atrapalha o executar.
func (a *Ativa) Conversa(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conversa != nil && a.conversa.Vivo() {
		return a.conversa, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal("", ""), codigoTerminal)
	if err != nil {
		return nil, err
	}
	if a.conversa, err = AbrirTerminal(ctx, s); err != nil {
		return nil, err
	}
	a.conversa.Ver(ctx, 25*time.Second) // até o prompt: o que chega antes se perde
	return a.conversa, nil
}

// TerminalAdmin roda como SYSTEM; exige usuário e senha de um administrador do Windows informados no conectar.
func (a *Ativa) TerminalAdmin(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admin != nil && a.admin.Vivo() {
		return a.admin, nil
	}
	if a.usuario == "" {
		return nil, errors.New("terminal de administrador precisa do usuário e senha de um administrador do Windows no conectar; sem isso, abra o PowerShell como administrador pela tela e clique Sim no aviso")
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal(a.usuario, a.senhaWin), codigoTerminal)
	if err != nil {
		return nil, err
	}
	a.admin, err = AbrirTerminal(ctx, s)
	return a.admin, err
}

// Tela da máquina, reaberta se caiu.
func (a *Ativa) Tela(ctx context.Context) (*Tela, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tela != nil && a.tela.Vivo() {
		return a.tela, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_DEFAULT_CONN, LoginTela, nil)
	if err != nil {
		return nil, err
	}
	a.tela = AbrirTela(s)
	return a.tela, nil
}

// Arquivos: sessão de transferência.
func (a *Ativa) Arquivos(ctx context.Context) (*files.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.arq != nil {
		if a.arq.Probe(ctx, 4*time.Second) == nil {
			return a.arq, nil
		}
		a.arq.Close()
		a.arq = nil
	}
	s, err := a.conectar(ctx, pb.ConnType_FILE_TRANSFER, files.LoginFields("", true), files.LoginErrorCode)
	if err != nil {
		return nil, err
	}
	a.arq = files.New(s, nil)
	return a.arq, nil
}

// Enviar grava conteúdo num arquivo da máquina.
func (a *Ativa) Enviar(ctx context.Context, conteudo []byte, destino string) error {
	c, err := a.Arquivos(ctx)
	if err != nil {
		return err
	}
	d, err := os.MkdirTemp("", "rdmcp")
	if err != nil {
		return err
	}
	defer os.RemoveAll(d)
	nome := c.Base(destino)
	local := filepath.Join(d, filepath.Base(nome))
	if err := os.WriteFile(local, conteudo, 0o600); err != nil {
		return err
	}
	return c.Upload(ctx, local, destino, files.TransferOptions{})
}

// Baixar traz um arquivo da máquina para o disco local.
func (a *Ativa) Baixar(ctx context.Context, origem, local string) error {
	c, err := a.Arquivos(ctx)
	if err != nil {
		return err
	}
	return c.Download(ctx, origem, local, files.TransferOptions{})
}

// Executar roda o comando no terminal do usuário (ou de administrador). Comando longo vai como arquivo temporário,
// e saída grande volta como arquivo; os dois são apagados da máquina em seguida.
func (a *Ativa) Executar(ctx context.Context, comando, pasta string, admin, utf8 bool, maxSaida int, prazo time.Duration) (Resultado, error) {
	abrir := a.Terminal
	if admin {
		abrir = a.TerminalAdmin
	}
	t, err := abrir(ctx)
	if err != nil {
		return Resultado{}, err
	}
	r, err := t.Executar(ctx, comando, pasta, maxSaida, utf8, prazo)
	if errors.Is(err, ErrComandoLongo) {
		r, err = a.executarArquivo(ctx, t, comando, pasta, maxSaida, utf8, prazo)
	}
	if err != nil || r.Arquivo == "" {
		return r, err
	}
	defer t.Executar(context.WithoutCancel(ctx), "Remove-Item -LiteralPath '"+strings.ReplaceAll(r.Arquivo, "'", "''")+"' -Force", "", 500, false, 20*time.Second)
	d, err := os.MkdirTemp("", "rdmcp")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(d)
	local := filepath.Join(d, "saida.txt")
	if err := a.Baixar(ctx, r.Arquivo, local); err != nil {
		return r, fmt.Errorf("a saída é grande e a máquina não deixou baixar o arquivo dela (%w); filtre no comando", err)
	}
	b, err := os.ReadFile(local)
	r.Saida, r.Arquivo = Aparar(string(b)), ""
	return r, err
}

func (a *Ativa) executarArquivo(ctx context.Context, t *Terminal, comando, pasta string, maxSaida int, utf8 bool, prazo time.Duration) (Resultado, error) {
	tmp, err := t.Executar(ctx, se(t.windows, "[IO.Path]::GetTempPath()", "echo ${TMPDIR:-/tmp}/"), "", 1000, false, 30*time.Second)
	if err != nil {
		return Resultado{}, err
	}
	destino := strings.TrimSpace(tmp.Saida) + "rdmcp-" + marca() + se(t.windows, ".ps1", ".sh")
	if err := a.Enviar(ctx, []byte(se(t.windows, string(rune(0xFEFF)), "")+comando), destino); err != nil {
		return Resultado{}, fmt.Errorf("comando longo: não consegui enviar o arquivo: %w", err)
	}
	if t.windows {
		q := "'" + strings.ReplaceAll(destino, "'", "''") + "'"
		return t.Executar(ctx, "try { & "+q+" } finally { Remove-Item -LiteralPath "+q+" -Force -ErrorAction SilentlyContinue }", pasta, maxSaida, utf8, prazo)
	}
	return t.Executar(ctx, "sh '"+destino+"'; c=$?; rm -f '"+destino+"'; (exit $c)", pasta, maxSaida, utf8, prazo)
}

// PedirAdmin envia o comando como script e pede a elevação por um processo solto, que sobrevive ao terminal. Quem
// confirma é quem está na tela (aviso do Windows). Devolve o id para AdminResultado. O processo elevado confere o
// resumo do arquivo antes de rodar: o que foi pedido é o que roda.
func (a *Ativa) PedirAdmin(ctx context.Context, comando string, utf8 bool) (string, error) {
	t, err := a.Terminal(ctx)
	if err != nil {
		return "", err
	}
	if !t.windows {
		return "", errors.New("só no Windows; no Linux/macOS use sudo no terminal interativo")
	}
	tmp, err := t.Executar(ctx, "[IO.Path]::GetTempPath()", "", 1000, false, 30*time.Second)
	if err != nil {
		return "", err
	}
	id := marca()
	base := strings.TrimSpace(tmp.Saida) + "rdmcp-" + id
	corpo := []byte(comando)
	if err := a.Enviar(ctx, corpo, base+".admin"); err != nil {
		return "", fmt.Errorf("não consegui enviar o comando: %w", err)
	}
	soma := sha256.Sum256(corpo)
	elevado := "$ProgressPreference='SilentlyContinue';$b=[IO.File]::ReadAllBytes('" + aspas(base) + ".admin');" +
		"$h=[BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($b)).Replace('-','');" +
		"if($h -ne '" + strings.ToUpper(hex.EncodeToString(soma[:])) + "'){exit 9};" +
		"try{[Console]::OutputEncoding=" + se(utf8, "[Text.Encoding]::UTF8", "[Text.Encoding]::GetEncoding([Globalization.CultureInfo]::CurrentCulture.TextInfo.OEMCodePage)") + "}catch{};" +
		"$global:LASTEXITCODE=0;$Error.Clear();$o='';try{$s=[ScriptBlock]::Create([Text.Encoding]::UTF8.GetString($b));$o=(& $s 2>&1|Out-String -Width 4096)}catch{$o=[string]$o+($_|Out-String)};" +
		"$k=$global:LASTEXITCODE;if($k -eq 0 -and @($Error|Where-Object{$_.FullyQualifiedErrorId -notlike 'NativeCommandError*'}).Count){$k=1};" +
		"[IO.File]::WriteAllText('" + aspas(base) + ".parte',[string]$k+[char]10+$o,(New-Object Text.UTF8Encoding $false));Move-Item '" + aspas(base) + ".parte' '" + aspas(base) + ".saida' -Force"
	pede := "Start-Process powershell -Verb RunAs -WindowStyle Hidden -ArgumentList '-NoProfile -ExecutionPolicy Bypass -EncodedCommand " + codificado(elevado) + "'"
	r, err := t.Executar(ctx, "Start-Process powershell -WindowStyle Hidden -ArgumentList '-NoProfile -EncodedCommand "+codificado(pede)+"'", "", 2000, false, 30*time.Second)
	if err != nil && !errors.Is(err, ErrComandoLongo) {
		return "", err
	}
	if errors.Is(err, ErrComandoLongo) {
		r, err = a.executarArquivo(ctx, t, "Start-Process powershell -WindowStyle Hidden -ArgumentList '-NoProfile -EncodedCommand "+codificado(pede)+"'", "", 2000, false, 30*time.Second)
		if err != nil {
			return "", err
		}
	}
	if r.Codigo != 0 {
		return "", errors.New(r.Saida)
	}
	a.mu.Lock()
	a.admins[id] = base
	a.mu.Unlock()
	return id, nil
}

// AdminResultado busca a saída do comando elevado e apaga os arquivos. pronto=false: ainda não confirmaram ou não acabou.
func (a *Ativa) AdminResultado(ctx context.Context, id string, max int) (saida string, codigo int, pronto bool, err error) {
	a.mu.Lock()
	base, ok := a.admins[id]
	a.mu.Unlock()
	if !ok {
		return "", 0, false, errors.New("pedido desconhecido")
	}
	q := "'" + aspas(base)
	r, err := a.Executar(ctx, "if(Test-Path -LiteralPath "+q+".saida'){Get-Content -Raw -Encoding UTF8 -LiteralPath "+q+".saida'}else{throw 'ainda nao'}", "", false, false, max, 60*time.Second)
	if err != nil {
		return "", 0, false, err
	}
	if r.Codigo != 0 {
		return "", 0, false, nil
	}
	cod, resto, _ := strings.Cut(r.Saida, "\n")
	codigo, _ = strconv.Atoi(strings.TrimSpace(cod))
	a.Executar(ctx, "Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath "+q+".saida',"+q+".admin',"+q+".parte'", "", false, false, 500, 30*time.Second)
	a.mu.Lock()
	delete(a.admins, id)
	a.mu.Unlock()
	return Aparar(resto), codigo, true, nil
}

// DesistirAdmin apaga o pedido que não foi confirmado.
func (a *Ativa) DesistirAdmin(ctx context.Context, id string) error {
	a.mu.Lock()
	base, ok := a.admins[id]
	delete(a.admins, id)
	a.mu.Unlock()
	if !ok {
		return errors.New("pedido desconhecido")
	}
	q := "'" + aspas(base)
	_, err := a.Executar(ctx, "Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath "+q+".saida',"+q+".admin',"+q+".parte'", "", false, false, 500, 30*time.Second)
	return err
}

func aspas(s string) string { return strings.ReplaceAll(s, "'", "''") }

func codificado(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func se(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// Discar abre uma conexão TCP a partir da máquina do cliente (a rede dele), pelo túnel do RustDesk.
func (a *Ativa) Discar(ctx context.Context, host string, porta int32) (net.Conn, error) {
	alvo := forward.Target{Host: host, Port: porta}
	a.mu.Lock()
	m, ok := a.tuneis[alvo.String()]
	if !ok {
		m = &forward.Mapping{Target: alvo, Mode: forward.ModeMux, Dial: func(ctx context.Context, t forward.Target, mux bool) (*peer.Session, error) {
			return a.conectar(ctx, pb.ConnType_PORT_FORWARD, forward.LoginFields(t, mux), forward.LoginErrorCode)
		}}
		a.tuneis[alvo.String()] = m
	}
	a.mu.Unlock()
	nosso, deles := net.Pipe()
	c := &discada{Conn: nosso}
	go func() {
		if err := m.Serve(context.WithoutCancel(ctx), deles); err != nil {
			e := fmt.Errorf("a máquina não abriu %s: %w", alvo, err)
			c.motivo.Store(&e)
		}
		deles.Close()
	}()
	return c, nil
}

// discada troca o "fim da conexão" seco pelo motivo real (porta fechada, nome que não resolve, túnel sem permissão).
type discada struct {
	net.Conn
	motivo atomic.Pointer[error]
}

func (d *discada) Read(b []byte) (int, error) {
	n, err := d.Conn.Read(b)
	if err != nil {
		if e := d.motivo.Load(); e != nil {
			return n, *e
		}
	}
	return n, err
}

// HTTP faz um pedido a partir da rede do cliente: sistema web interno, API local.
func (a *Ativa) HTTP(ctx context.Context, req *http.Request, max int64) (*http.Response, []byte, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		n, err := net.LookupPort("tcp", p)
		if err != nil {
			return nil, err
		}
		return a.Discar(ctx, h, int32(n))
	}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return resp, b, err
}

// Salvar guarda o acesso fixo no cadastro. Antes entra de novo como outra sessão: a senha de uso único, que já
// morreu na primeira entrada, é recusada aqui em vez de virar um cadastro que não funciona.
func (a *Ativa) Salvar(ctx context.Context, apelido, obs string) error {
	a.cred.Lock()
	hash := a.hash
	a.cred.Unlock()
	prova := &Ativa{ID: a.ID, g: a.g, hash: hash, sessao: peer.NovaSessao()}
	s, err := prova.conectar(ctx, pb.ConnType_FILE_TRANSFER, files.LoginFields("", false), files.LoginErrorCode)
	if err != nil {
		return fmt.Errorf("só dá para salvar com a senha FIXA, e uma entrada nova com esta senha falhou: %w", err)
	}
	s.Close("")
	a.cred.Lock()
	m := maquinas.Maquina{Apelido: apelido, ID: a.ID, Cliente: a.Cliente, Autorizado: a.Autorizado, Em: time.Now(), Obs: obs, Hash: a.hash}
	a.cred.Unlock()
	if err := a.g.Cadastro.Salvar(m); err != nil {
		return err
	}
	a.Apelido = apelido
	return nil
}

// Fechar encerra tudo o que está aberto com a máquina.
func (a *Ativa) Fechar() {
	a.g.mu.Lock()
	if a.g.ativas[a.ID] == a {
		delete(a.g.ativas, a.ID)
	}
	a.g.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.term != nil {
		a.term.Fechar()
	}
	if a.admin != nil {
		a.admin.Fechar()
	}
	if a.conversa != nil {
		a.conversa.Fechar()
	}
	if a.tela != nil {
		a.tela.Fechar()
	}
	if a.arq != nil {
		a.arq.Close()
	}
	for _, m := range a.tuneis {
		m.Close()
	}
	a.cred.Lock()
	a.senha, a.senhaWin, a.hash = "", "", nil
	a.cred.Unlock()
}
