// Package sessao mantém as sessões abertas com a máquina do cliente: terminal e tela.
package sessao

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"crypto/rand"
	"encoding/hex"

	"github.com/klauspost/compress/zstd"

	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/peer"
)

const (
	terminalID   = 1
	colunas      = 220
	linhas       = 50
	guardaSaida  = 1 << 20 // último 1 MB da saída crua
	limiteLinha  = 7000    // acima disso o comando vai por arquivo, não pela linha
	esperaAbrir  = 25 * time.Second
	batimento    = 15 * time.Second
	cutucaWindow = 2 * time.Second
)

// Terminal é um shell aberto na máquina remota, com a saída guardada para ler aos poucos.
type Terminal struct {
	s       *peer.Session
	cancel  context.CancelFunc
	windows bool

	mu     sync.Mutex
	saida  []byte // saída crua acumulada (janela de guardaSaida)
	inicio int64  // posição absoluta do primeiro byte de saida
	lido   int64  // até onde o Ler já entregou
	fim    error  // motivo do fim, quando acabou
	sinal  chan struct{}

	exec sync.Mutex // um Executar por vez
}

// LoginTerminal preenche o pedido de login. Com usuário do Windows, o shell roda como administrador (SYSTEM).
func LoginTerminal(usuario, senha string) func(*pb.LoginRequest) {
	return func(lr *pb.LoginRequest) {
		if usuario != "" {
			lr.OsLogin = &pb.OSLogin{Username: usuario, Password: senha}
		}
		lr.Union = &pb.LoginRequest_Terminal{Terminal: &pb.Terminal{}}
	}
}

// AbrirTerminal abre o shell numa sessão já logada.
func AbrirTerminal(ctx context.Context, s *peer.Session) (*Terminal, error) {
	run, cancel := context.WithCancel(context.Background())
	t := &Terminal{s: s, cancel: cancel, sinal: make(chan struct{}), windows: strings.EqualFold(s.PeerInfo.GetPlatform(), "Windows")}
	aberto := make(chan error, 1)
	go t.ler(aberto)
	if err := t.acao(&pb.TerminalAction{Union: &pb.TerminalAction_Open{Open: &pb.OpenTerminal{TerminalId: terminalID, Rows: linhas, Cols: colunas}}}); err != nil {
		cancel()
		return nil, err
	}
	select {
	case err := <-aberto:
		if err != nil {
			cancel()
			s.Close("")
			return nil, err
		}
	case <-time.After(esperaAbrir):
		cancel()
		s.Close("")
		return nil, errors.New("a máquina não abriu o terminal")
	case <-ctx.Done():
		cancel()
		s.Close("")
		return nil, ctx.Err()
	}
	go s.KeepAlive(run, batimento)
	if t.windows {
		go t.cutucar(run)
	}
	return t, nil
}

func (t *Terminal) acao(a *pb.TerminalAction) error {
	return t.s.Send(&pb.Message{Union: &pb.Message_TerminalAction{TerminalAction: a}})
}

// cutucar: no Windows (1.5.3) o fim do shell só é avisado quando chega mais entrada. Um resize igual resolve.
func (t *Terminal) cutucar(ctx context.Context) {
	tk := time.NewTicker(cutucaWindow)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			if t.acao(&pb.TerminalAction{Union: &pb.TerminalAction_Resize{Resize: &pb.ResizeTerminal{TerminalId: terminalID, Rows: linhas, Cols: colunas}}}) != nil {
				return
			}
		}
	}
}

var descompacta, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderConcurrency(1))

func dados(d *pb.TerminalData) ([]byte, error) {
	if !d.GetCompressed() {
		return d.GetData(), nil
	}
	return descompacta.DecodeAll(d.GetData(), nil)
}

func (t *Terminal) ler(aberto chan error) {
	avisou := false
	avisa := func(err error) {
		if !avisou {
			avisou = true
			aberto <- err
		}
	}
	for {
		m, err := t.s.Recv()
		if err != nil {
			avisa(err)
			t.encerra(err)
			return
		}
		r := m.GetTerminalResponse()
		if r == nil {
			continue
		}
		switch u := r.Union.(type) {
		case *pb.TerminalResponse_Opened:
			if !u.Opened.GetSuccess() {
				msg := u.Opened.GetMessage()
				if msg == "" {
					msg = "a máquina recusou abrir o terminal"
				}
				avisa(errors.New(msg))
				return
			}
			avisa(nil)
		case *pb.TerminalResponse_Data:
			d, err := dados(u.Data)
			if err == nil {
				t.guarda(d)
			}
		case *pb.TerminalResponse_Closed:
			avisa(errors.New("o shell fechou"))
			t.encerra(fmt.Errorf("o shell terminou (código %d)", u.Closed.GetExitCode()))
			return
		case *pb.TerminalResponse_Error:
			avisa(errors.New(u.Error.GetMessage()))
			t.encerra(errors.New(u.Error.GetMessage()))
			return
		}
	}
}

func (t *Terminal) guarda(b []byte) {
	t.mu.Lock()
	t.saida = append(t.saida, b...)
	if n := len(t.saida) - guardaSaida; n > 0 {
		t.saida = append([]byte(nil), t.saida[n:]...)
		t.inicio += int64(n)
	}
	t.avisaMu()
	t.mu.Unlock()
}

func (t *Terminal) encerra(err error) {
	t.mu.Lock()
	if t.fim == nil {
		t.fim = err
	}
	t.avisaMu()
	t.mu.Unlock()
	t.cancel()
}

func (t *Terminal) avisaMu() { close(t.sinal); t.sinal = make(chan struct{}) }

// Escrever manda texto cru para o shell (interativo: sqlcmd, isql, instalador de console...).
func (t *Terminal) Escrever(texto string) error {
	t.mu.Lock()
	fim := t.fim
	t.mu.Unlock()
	if fim != nil {
		return fim
	}
	if t.windows {
		texto = strings.ReplaceAll(strings.ReplaceAll(texto, "\r\n", "\r"), "\n", "\r")
	}
	b := []byte(texto)
	for len(b) > 0 {
		n := min(len(b), 4096)
		if err := t.acao(&pb.TerminalAction{Union: &pb.TerminalAction_Data{Data: &pb.TerminalData{TerminalId: terminalID, Data: b[:n]}}}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// Ler devolve o que saiu desde a última leitura, limpo de sequências de tela. Espera até `espera` por novidade.
func (t *Terminal) Ler(ctx context.Context, espera time.Duration) (string, error) {
	prazo := time.NewTimer(espera)
	defer prazo.Stop()
	for {
		t.mu.Lock()
		if t.lido < t.inicio {
			t.lido = t.inicio
		}
		novo := append([]byte(nil), t.saida[t.lido-t.inicio:]...)
		sinal, fim := t.sinal, t.fim
		if len(novo) > 0 {
			t.lido += int64(len(novo))
			t.mu.Unlock()
			return Limpar(string(novo)), nil
		}
		t.mu.Unlock()
		if fim != nil {
			return "", fim
		}
		select {
		case <-sinal:
		case <-prazo.C:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Resultado de um comando: a saída inteira e o código de saída.
type Resultado struct {
	Saida  string
	Codigo int
	// Cortada: a saída passou do limite e só o começo e o fim vieram.
	Cortada bool
}

func marca() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Executar roda um comando do PowerShell (Windows) ou sh (Linux/macOS) e devolve saída e código.
// O comando viaja codificado e a saída volta em base64 entre marcas: o eco do console, o prompt e a quebra
// de linha do terminal não entram no resultado.
func (t *Terminal) Executar(ctx context.Context, comando, pasta string, maxSaida int, prazo time.Duration) (Resultado, error) {
	t.exec.Lock()
	defer t.exec.Unlock()
	id := marca()
	var linha string
	if t.windows {
		linha = linhaPowerShell(id, comando, pasta, maxSaida)
	} else {
		linha = linhaSh(id, comando, pasta, maxSaida)
	}
	if len(linha) > limiteLinha {
		return Resultado{}, ErrComandoLongo
	}
	return t.rodar(ctx, id, linha, prazo)
}

// ErrComandoLongo: o comando não cabe numa linha do console; mande como arquivo e execute o arquivo.
var ErrComandoLongo = errors.New("comando longo demais para uma linha: envie como arquivo (.ps1) e execute o arquivo")

func (t *Terminal) rodar(ctx context.Context, id, linha string, prazo time.Duration) (Resultado, error) {
	t.mu.Lock()
	de := t.inicio + int64(len(t.saida))
	t.mu.Unlock()
	if err := t.Escrever(linha + "\n"); err != nil {
		return Resultado{}, err
	}
	tempo := time.NewTimer(prazo)
	defer tempo.Stop()
	for {
		t.mu.Lock()
		if de < t.inicio {
			de = t.inicio
		}
		bruto := string(t.saida[de-t.inicio:])
		sinal, fim := t.sinal, t.fim
		t.mu.Unlock()
		if r, ok, err := extrair(bruto, id); ok {
			t.mu.Lock()
			t.lido = t.inicio + int64(len(t.saida)) // já entregue: o Ler não repete
			t.mu.Unlock()
			return r, err
		}
		if fim != nil {
			return Resultado{Saida: Limpar(bruto)}, fim
		}
		select {
		case <-sinal:
		case <-tempo.C:
			// Console ocupado estragaria o próximo comando: interrompe e fecha; o próximo abre outro.
			t.Escrever("\x03")
			time.Sleep(700 * time.Millisecond)
			t.Fechar()
			return Resultado{Saida: ultimas(Limpar(bruto), 4000), Codigo: -1}, fmt.Errorf("passou de %s e foi interrompido; para tarefa longa aumente prazo_s ou use terminal_escrever", prazo)
		case <-ctx.Done():
			return Resultado{}, ctx.Err()
		}
	}
}

func linhaPowerShell(id, comando, pasta string, maxSaida int) string {
	cd := ""
	if pasta != "" {
		cd = "Set-Location -LiteralPath '" + strings.ReplaceAll(pasta, "'", "''") + "';"
	}
	// $LASTEXITCODE cobre programa externo; $? cobre cmdlet. A saída volta em base64 UTF-8.
	script := "$ProgressPreference='SilentlyContinue';$ErrorActionPreference='Continue';" +
		"try{[Console]::OutputEncoding=[Text.Encoding]::UTF8}catch{};" + cd +
		"$global:LASTEXITCODE=0;$ok=$true;" +
		"try{$o=(& {" + comando + "\n} 2>&1|Out-String -Width 4096);$ok=$?}catch{$o=($o+($_|Out-String));$ok=$false};" +
		"$c=$LASTEXITCODE;if($c -eq 0 -and -not $ok){$c=1};" +
		"$m=" + strconv.Itoa(maxSaida) + ";$x=0;if($o.Length -gt $m){$h=[int]($m/2);$o=$o.Substring(0,$h)+\"`n[...]`n\"+$o.Substring($o.Length-$h);$x=1};" +
		"$b=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$o));" +
		"Write-Output ('<<'+'RDI:" + id + ":'+$x+'>>');for($i=0;$i -lt $b.Length;$i+=200){Write-Output $b.Substring($i,[Math]::Min(200,$b.Length-$i))};" +
		"Write-Output ('<<'+'RDF:" + id + ":'+$c+'>>')"
	return "powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + base64.StdEncoding.EncodeToString(utf16le(script))
}

func linhaSh(id, comando, pasta string, maxSaida int) string {
	cd := ""
	if pasta != "" {
		cd = "cd '" + strings.ReplaceAll(pasta, "'", `'\''`) + "' && "
	}
	dentro := base64.StdEncoding.EncodeToString([]byte(cd + "{ " + comando + "\n}"))
	m := strconv.Itoa(maxSaida)
	script := "o=$(echo " + dentro + " | base64 -d | sh 2>&1); c=$?; x=0; " +
		"if [ ${#o} -gt " + m + " ]; then o=$(printf %s \"$o\" | head -c " + m + "); x=1; fi; " +
		"echo \"<<\"\"RDI:" + id + ":$x>>\"; printf %s \"$o\" | base64 | fold -w 200; echo; echo \"<<\"\"RDF:" + id + ":$c>>\""
	return "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh"
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// extrair acha a resposta entre as marcas no meio da saída crua do console.
func extrair(bruto, id string) (Resultado, bool, error) {
	limpo := Limpar(bruto)
	reI := regexp.MustCompile(`<<RDI:` + id + `:([01])>>`)
	reF := regexp.MustCompile(`<<RDF:` + id + `:(-?\d+)>>`)
	i := reI.FindStringSubmatchIndex(limpo)
	if i == nil {
		return Resultado{}, false, nil
	}
	f := reF.FindStringSubmatchIndex(limpo[i[1]:])
	if f == nil {
		return Resultado{}, false, nil
	}
	corpo := limpo[i[1] : i[1]+f[0]]
	codigo, _ := strconv.Atoi(limpo[i[1]+f[2] : i[1]+f[3]])
	b64 := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			return r
		}
		return -1
	}, corpo)
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Resultado{Codigo: codigo}, true, fmt.Errorf("a saída chegou corrompida (%v); rode de novo", err)
	}
	return Resultado{Saida: strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n "), Codigo: codigo, Cortada: limpo[i[2]:i[3]] == "1"}, true, nil
}

var (
	reOSC = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")
	reCSI = regexp.MustCompile("\x1b\\[[0-9;?<=>!]*[ -/]*[@-~]")
	reESC = regexp.MustCompile("\x1b[()][A-Za-z0-9]|\x1b[=>78DEHMNOZc]")
)

// Limpar tira as sequências de tela do terminal, deixando o texto.
func Limpar(s string) string {
	s = reOSC.ReplaceAllString(s, "")
	s = reCSI.ReplaceAllString(s, "")
	s = reESC.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\r' || (r < 32 && r != '\n' && r != '\t') || r == 127 {
			return -1
		}
		return r
	}, s)
}

func ultimas(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}

// Vivo diz se o shell ainda está aberto.
func (t *Terminal) Vivo() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.fim == nil }

// Fechar encerra o shell e a conexão.
func (t *Terminal) Fechar() {
	t.acao(&pb.TerminalAction{Union: &pb.TerminalAction_Close{Close: &pb.CloseTerminal{TerminalId: terminalID}}})
	t.s.Close("")
	t.encerra(errors.New("terminal fechado"))
}
