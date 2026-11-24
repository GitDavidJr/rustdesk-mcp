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

	"crypto/rand"
	"encoding/hex"

	"github.com/hinshun/vt10x"
	"github.com/klauspost/compress/zstd"

	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/peer"
)

const (
	terminalID  = 1
	colunas     = 220
	linhas      = 50
	guardaSaida = 1 << 20 // último 1 MB da saída crua
	limiteLinha = 7000    // acima disso o comando vai por arquivo, não pela linha
	esperaAbrir = 25 * time.Second
	batimento   = 15 * time.Second
)

// Terminal é um shell aberto na máquina remota, com a saída guardada para ler aos poucos.
type Terminal struct {
	s       *peer.Session
	cancel  context.CancelFunc
	windows bool
	pronto  bool // a função auxiliar já está carregada (sob exec)

	mu     sync.Mutex
	saida  []byte // saída crua acumulada (janela de guardaSaida)
	inicio int64  // posição absoluta do primeiro byte de saida
	vt     vt10x.Terminal
	fim    error // motivo do fim, quando acabou
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
	t := &Terminal{s: s, cancel: cancel, sinal: make(chan struct{}), vt: vt10x.New(vt10x.WithSize(colunas, linhas)), windows: strings.EqualFold(s.PeerInfo.GetPlatform(), "Windows")}
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
	return t, nil
}

func (t *Terminal) acao(a *pb.TerminalAction) error {
	return t.s.Send(&pb.Message{Union: &pb.Message_TerminalAction{TerminalAction: a}})
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
	t.emular(b)
	if n := len(t.saida) - guardaSaida; n > 0 {
		t.saida = append([]byte(nil), t.saida[n:]...)
		t.inicio += int64(n)
	}
	t.avisaMu()
	t.mu.Unlock()
}

// emular nunca derruba a sessão: se o emulador engasgar com uma sequência estranha, recomeça vazio.
func (t *Terminal) emular(b []byte) {
	defer func() {
		if recover() != nil {
			t.vt = vt10x.New(vt10x.WithSize(colunas, linhas))
		}
	}()
	t.vt.Write(b)
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

// Ver espera a tela do terminal aquietar (até `espera`) e devolve o que ela mostra, como uma pessoa veria.
func (t *Terminal) Ver(ctx context.Context, espera time.Duration) (string, error) {
	const quieto = 700 * time.Millisecond
	limite := time.Now().Add(espera)
	desde := t.posicao()
	mudou := false
	ultima := time.Now()
	for {
		t.mu.Lock()
		pos, sinal, fim := t.inicio+int64(len(t.saida)), t.sinal, t.fim
		t.mu.Unlock()
		if pos != desde {
			desde, mudou, ultima = pos, true, time.Now()
		}
		if fim != nil {
			return t.emulada(), fim
		}
		falta := time.Until(limite)
		if mudou && t.emulada() != "" { // a primeira rajada do console só limpa a tela
			falta = min(falta, quieto-time.Since(ultima))
		}
		if falta <= 0 {
			return t.emulada(), nil
		}
		tempo := time.NewTimer(falta)
		select {
		case <-sinal:
		case <-tempo.C:
		case <-ctx.Done():
			tempo.Stop()
			return "", ctx.Err()
		}
		tempo.Stop()
	}
}

func (t *Terminal) emulada() string {
	t.mu.Lock()
	txt := t.vt.String()
	t.mu.Unlock()
	ls := strings.Split(txt, "\n")
	for i := range ls {
		ls[i] = strings.TrimRight(ls[i], " \x00")
	}
	return strings.Trim(strings.Join(ls, "\n"), "\n")
}

// Resultado de um comando: a saída inteira e o código de saída.
type Resultado struct {
	Saida  string
	Codigo int
	// Cortada: a saída passou do limite e só o começo e o fim vieram.
	Cortada bool
	// Arquivo: a saída ficou num arquivo temporário da máquina (saída grande no Windows).
	Arquivo string
}

func marca() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Executar roda um comando do PowerShell (Windows) ou sh (Linux/macOS) e devolve saída e código.
// O comando viaja em base64 e a saída volta em base64 entre marcas: o eco do console, o prompt e a quebra de
// linha do terminal não entram no resultado. No Windows, saída grande vai para um arquivo temporário da máquina
// (Resultado.Arquivo): o console redesenha a tela quando rola depressa e embaralha o texto.
func (t *Terminal) Executar(ctx context.Context, comando, pasta string, maxSaida int, utf8 bool, prazo time.Duration) (Resultado, error) {
	t.exec.Lock()
	defer t.exec.Unlock()
	id := marca()
	var linha string
	if t.windows {
		if !t.pronto {
			if err := t.preparar(ctx); err != nil {
				return Resultado{}, err
			}
		}
		linha = linhaPowerShell(id, comando, pasta, maxSaida, utf8)
	} else {
		linha = linhaSh(id, comando, pasta, maxSaida)
	}
	if len(linha) > limiteLinha {
		return Resultado{}, ErrComandoLongo
	}
	return t.rodar(ctx, id, linha, prazo)
}

// ErrComandoLongo: o comando não cabe numa linha do console; mande como arquivo e execute o arquivo.
var ErrComandoLongo = errors.New("comando longo demais para uma linha: envie como arquivo e execute o arquivo")

// auxiliar fica carregada no PowerShell do terminal; cada comando só a chama. Uma linha só, sem aspas tipográficas.
// Código: o do programa externo ou, se um cmdlet falhou, 1 (texto de erro de programa externo não conta como falha).
const auxiliar = `function __rd($i,$c,$m,$u){$ProgressPreference='SilentlyContinue';$Error.Clear();try{$e=[Text.Encoding]::UTF8;if(-not $u){$e=[Text.Encoding]::GetEncoding([Globalization.CultureInfo]::CurrentCulture.TextInfo.OEMCodePage)};[Console]::OutputEncoding=$e}catch{};$global:LASTEXITCODE=0;$o='';` +
	`try{$s=[ScriptBlock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($c)));$o=(& $s 2>&1|Out-String -Width 4096)}catch{$o=[string]$o+($_|Out-String)};` +
	`$k=$global:LASTEXITCODE;if($k -eq $null){$k=0};if($k -eq 0 -and @($Error|Where-Object{$_.FullyQualifiedErrorId -notlike 'NativeCommandError*'}).Count){$k=1};` +
	`$o=[string]$o;$x=0;if($o.Length -gt $m){$h=[int]($m/2);$o=$o.Substring(0,$h)+[char]10+'[...]'+[char]10+$o.Substring($o.Length-$h);$x=1};` +
	`$f=0;if($o.Length -gt 3000){$f=1;$p=[IO.Path]::GetTempPath()+'rdmcp-'+$i+'.txt';[IO.File]::WriteAllText($p,$o,(New-Object Text.UTF8Encoding $false));$o=$p};` +
	`$b=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($o));$n=0;$l=[char]60+[char]60;` +
	`[Console]::WriteLine($l+'RDI:'+$i+':'+$x+$f+'>>');for($q=0;$q -lt $b.Length;$q+=160){[Console]::WriteLine('~'+$n+'~'+$b.Substring($q,[Math]::Min(160,$b.Length-$q))+'~');$n++};` +
	`[Console]::WriteLine($l+'RDF:'+$i+':'+$k+':'+$n+'>>')}`

// preparar abre um PowerShell limpo dentro do shell padrão (que pode ser cmd ou pwsh), sem o PSReadLine, e carrega a
// função auxiliar. Espera o shell interno avisar que está pronto: o PSReadLine do shell de fora lê tudo o que chega
// antes da hora, e o que vem junto se perde.
func (t *Terminal) preparar(ctx context.Context) error {
	id := marca()
	abre := "powershell -NoLogo -NoProfile -ExecutionPolicy Bypass -NoExit -Command \"Remove-Module PSReadLine -ErrorAction SilentlyContinue; [Console]::WriteLine([string][char]60+[char]60+'RDP:" + id + ">>')\""
	de := t.posicao()
	if err := t.Escrever(abre + "\n"); err != nil {
		return err
	}
	if err := t.esperar(ctx, de, "<<RDP:"+id+">>", 45*time.Second); err != nil {
		return fmt.Errorf("o PowerShell da máquina não abriu: %w", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := t.Escrever(auxiliar + "\n"); err != nil {
		return err
	}
	if _, err := t.rodar(ctx, id, linhaPowerShell(id, "1", "", 100, false), 30*time.Second); err != nil {
		return fmt.Errorf("o PowerShell da máquina não respondeu: %w", err)
	}
	t.pronto = true
	return nil
}

func (t *Terminal) posicao() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inicio + int64(len(t.saida))
}

// esperar até o texto aparecer na saída depois da posição dada.
func (t *Terminal) esperar(ctx context.Context, de int64, texto string, prazo time.Duration) error {
	tempo := time.NewTimer(prazo)
	defer tempo.Stop()
	for {
		t.mu.Lock()
		bruto := string(t.saida[max(de, t.inicio)-t.inicio:])
		sinal, fim := t.sinal, t.fim
		t.mu.Unlock()
		if strings.Contains(Limpar(bruto), texto) {
			return nil
		}
		if fim != nil {
			return fim
		}
		select {
		case <-sinal:
		case <-tempo.C:
			return fmt.Errorf("sem resposta em %s; última saída: %q", prazo, ultimas(strings.TrimSpace(Limpar(bruto)), 300))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *Terminal) rodar(ctx context.Context, id, linha string, prazo time.Duration) (Resultado, error) {
	de := t.posicao()
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
			return r, err
		}
		if fim != nil {
			return Resultado{Saida: ultimas(Limpar(bruto), 4000), Codigo: -1}, fim
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

func linhaPowerShell(id, comando, pasta string, maxSaida int, utf8 bool) string {
	if pasta != "" {
		comando = "Set-Location -LiteralPath '" + strings.ReplaceAll(pasta, "'", "''") + "'\n" + comando
	}
	return "__rd '" + id + "' '" + base64.StdEncoding.EncodeToString([]byte(comando)) + "' " + strconv.Itoa(maxSaida) + se(utf8, " 1", " 0")
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
		"printf '%s%s\\n' '<<' \"RDI:" + id + ":${x}0>>\"; printf %s \"$o\" | base64 | tr -d '\\n' | fold -w 160 | awk '{print \"~\" NR-1 \"~\" $0 \"~\"} END{print \"<\" \"<RDF:" + id + ":'$c':\" NR \">>\"}'"
	return "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh"
}

var rePedaco = regexp.MustCompile(`~(\d+)~([A-Za-z0-9+/=]+)~`)

// extrair acha a resposta entre as marcas no meio da saída crua do console. Os pedaços são numerados: repintura
// da tela repete pedaço, e pedaço que falta vira erro em vez de saída errada.
func extrair(bruto, id string) (Resultado, bool, error) {
	limpo := Limpar(bruto)
	reI := regexp.MustCompile(`<<RDI:` + id + `:([01])([01])>>`)
	reF := regexp.MustCompile(`<<RDF:` + id + `:(-?\d+):(\d+)>>`)
	i := reI.FindStringSubmatchIndex(limpo)
	if i == nil {
		return Resultado{}, false, nil
	}
	f := reF.FindStringSubmatchIndex(limpo[i[1]:])
	if f == nil {
		return Resultado{}, false, nil
	}
	resto := limpo[i[1]:]
	r := Resultado{Cortada: limpo[i[2]:i[3]] == "1"}
	r.Codigo, _ = strconv.Atoi(resto[f[2]:f[3]])
	total, _ := strconv.Atoi(resto[f[4]:f[5]])
	pedacos := make([]string, total)
	junto := strings.ReplaceAll(strings.ReplaceAll(resto[:f[0]], "\n", ""), " ", "")
	for _, m := range rePedaco.FindAllStringSubmatch(junto, -1) {
		if k, _ := strconv.Atoi(m[1]); k < total && len(m[2]) > len(pedacos[k]) {
			pedacos[k] = m[2]
		}
	}
	for k, p := range pedacos {
		if p == "" || (k < total-1 && len(p) != 160) {
			return r, true, fmt.Errorf("a saída chegou incompleta (pedaço %d de %d); rode de novo", k+1, total)
		}
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(pedacos, ""))
	if err != nil {
		return r, true, fmt.Errorf("a saída chegou corrompida (%v); rode de novo", err)
	}
	if limpo[i[4]:i[5]] == "1" {
		r.Arquivo = string(b)
		return r, true, nil
	}
	r.Saida = Aparar(string(b))
	return r, true, nil
}

// Aparar uniformiza quebras de linha e tira o espaço do fim.
func Aparar(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n ")
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
