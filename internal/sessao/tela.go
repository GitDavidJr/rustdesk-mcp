package sessao

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/image/draw"

	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/peer"
)

const (
	prazoPrint   = 20 * time.Second
	larguraPadra = 1568 // o que o modelo lê sem reduzir
)

// Tela é a sessão de área de trabalho: print sob demanda, mouse e teclado. O vídeo que o RustDesk manda é
// descartado (só confirmamos o recebimento); o print é o pedido nativo, que volta em PNG sem perda.
type Tela struct {
	s      *peer.Session
	cancel context.CancelFunc

	mu      sync.Mutex
	telas   []*pb.DisplayInfo
	escala  map[int]float64 // pixel do monitor por pixel da última imagem entregue
	espera  chan *pb.ScreenshotResponse
	fim     error
	avisos  []string // mensagens da máquina (permissão, UAC...)
	pedido  sync.Mutex
	proximo int
}

// LoginTela pede a sessão de tela com o mínimo de vídeo possível.
func LoginTela(lr *pb.LoginRequest) {
	lr.Option = &pb.OptionMessage{
		ImageQuality:     pb.ImageQuality_Low,
		DisableAudio:     pb.OptionMessage_Yes,
		DisableClipboard: pb.OptionMessage_Yes,
		CustomFps:        5,
		SupportedDecoding: &pb.SupportedDecoding{
			AbilityVp8: 1, AbilityVp9: 1, Prefer: pb.SupportedDecoding_VP8,
		},
	}
}

// AbrirTela começa a receber da sessão já logada.
func AbrirTela(s *peer.Session) *Tela {
	run, cancel := context.WithCancel(context.Background())
	t := &Tela{s: s, cancel: cancel, escala: map[int]float64{}, telas: s.PeerInfo.GetDisplays()}
	go t.ler()
	go s.KeepAlive(run, batimento)
	return t
}

func (t *Tela) ler() {
	for {
		m, err := t.s.Recv()
		if err != nil {
			t.mu.Lock()
			if t.fim == nil {
				t.fim = err
			}
			if t.espera != nil {
				close(t.espera)
				t.espera = nil
			}
			t.mu.Unlock()
			t.cancel()
			return
		}
		switch u := m.Union.(type) {
		case *pb.Message_VideoFrame:
			t.s.Send(&pb.Message{Union: &pb.Message_Misc{Misc: &pb.Misc{Union: &pb.Misc_VideoReceived{VideoReceived: true}}}})
		case *pb.Message_ScreenshotResponse:
			t.mu.Lock()
			if t.espera != nil {
				t.espera <- u.ScreenshotResponse
				t.espera = nil
			}
			t.mu.Unlock()
		case *pb.Message_PeerInfo:
			if d := u.PeerInfo.GetDisplays(); len(d) > 0 {
				t.mu.Lock()
				t.telas = d
				t.mu.Unlock()
			}
		case *pb.Message_Misc:
			switch x := u.Misc.Union.(type) {
			case *pb.Misc_PermissionInfo:
				t.aviso(fmt.Sprintf("permissão %s: %v", x.PermissionInfo.GetPermission(), x.PermissionInfo.GetEnabled()))
			case *pb.Misc_Uac:
				if x.Uac {
					t.aviso("o Windows abriu um aviso de administrador (UAC)")
				}
			case *pb.Misc_ForegroundWindowElevated:
				if x.ForegroundWindowElevated {
					t.aviso("a janela da frente é de administrador")
				}
			}
		case *pb.Message_MessageBox:
			t.aviso(strings.TrimSpace(u.MessageBox.GetTitle() + " " + u.MessageBox.GetText()))
		}
	}
}

func (t *Tela) aviso(s string) {
	t.mu.Lock()
	if n := len(t.avisos); n == 0 || t.avisos[n-1] != s {
		t.avisos = append(t.avisos, s)
	}
	if n := len(t.avisos); n > 10 {
		t.avisos = t.avisos[n-10:]
	}
	t.mu.Unlock()
}

// Avisos devolve e limpa o que a máquina avisou.
func (t *Tela) Avisos() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.avisos
	t.avisos = nil
	return a
}

// Monitores da máquina.
func (t *Tela) Monitores() []*pb.DisplayInfo { t.mu.Lock(); defer t.mu.Unlock(); return t.telas }

// Captura é a imagem entregue ao modelo.
type Captura struct {
	Dados           []byte
	Mime            string
	Largura, Altura int     // da imagem entregue
	Escala          float64 // pixels do monitor por pixel da imagem
	Monitor         int
}

// Recorte em pixels da imagem inteira (a última entregue), para ver um pedaço em resolução cheia.
type Recorte struct{ X, Y, Largura, Altura int }

// Capturar pede um print do monitor.
func (t *Tela) Capturar(ctx context.Context, monitor, larguraMax int, rec *Recorte) (*Captura, error) {
	t.pedido.Lock()
	defer t.pedido.Unlock()
	if larguraMax <= 0 {
		larguraMax = larguraPadra
	}
	espera := make(chan *pb.ScreenshotResponse, 1)
	t.mu.Lock()
	if t.fim != nil {
		t.mu.Unlock()
		return nil, t.fim
	}
	t.espera = espera
	t.proximo++
	sid := fmt.Sprintf("p%d", t.proximo)
	t.mu.Unlock()
	if err := t.s.Send(&pb.Message{Union: &pb.Message_ScreenshotRequest{ScreenshotRequest: &pb.ScreenshotRequest{Display: int32(monitor), Sid: sid}}}); err != nil {
		return nil, err
	}
	prazo := time.NewTimer(prazoPrint)
	defer prazo.Stop()
	var r *pb.ScreenshotResponse
	select {
	case x, ok := <-espera:
		if !ok {
			return nil, errors.New("a conexão caiu durante o print")
		}
		r = x
	case <-prazo.C:
		t.mu.Lock()
		t.espera = nil
		t.mu.Unlock()
		return nil, errors.New("a máquina não devolveu o print em 20 s")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.GetMsg() != "" {
		return nil, errors.New(r.GetMsg())
	}
	img, err := png.Decode(bytes.NewReader(r.GetData()))
	if err != nil {
		return nil, fmt.Errorf("print ilegível: %w", err)
	}
	t.mu.Lock()
	anterior := t.escala[monitor]
	t.mu.Unlock()
	return t.montar(img, monitor, larguraMax, rec, anterior)
}

func (t *Tela) montar(img image.Image, monitor, larguraMax int, rec *Recorte, escalaAtual float64) (*Captura, error) {
	b := img.Bounds()
	area := b
	if rec != nil {
		if escalaAtual == 0 {
			escalaAtual = float64(max(b.Dx(), 1)) / float64(min(b.Dx(), larguraPadra))
		}
		f := func(v int) int { return int(float64(v) * escalaAtual) }
		area = image.Rect(f(rec.X), f(rec.Y), f(rec.X+rec.Largura), f(rec.Y+rec.Altura)).Add(b.Min).Intersect(b)
		if area.Empty() {
			return nil, errors.New("recorte fora da imagem")
		}
	}
	w, h := area.Dx(), area.Dy()
	esc := 1.0
	if w > larguraMax {
		esc = float64(w) / float64(larguraMax)
		w, h = larguraMax, int(float64(h)/esc)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if esc == 1 {
		draw.Draw(dst, dst.Bounds(), img, area.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, area, draw.Src, nil)
	}
	if rec == nil { // o recorte não muda o sistema de coordenadas dos cliques
		t.mu.Lock()
		t.escala[monitor] = esc
		t.mu.Unlock()
	}
	var buf bytes.Buffer
	mime := "image/png"
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	if buf.Len() > 1_200_000 { // foto de fundo, vídeo: JPEG para não pesar a conversa
		buf.Reset()
		mime = "image/jpeg"
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
	}
	return &Captura{Dados: buf.Bytes(), Mime: mime, Largura: w, Altura: h, Escala: esc, Monitor: monitor}, nil
}

// ponto converte a coordenada da última imagem para a coordenada absoluta da área de trabalho.
func (t *Tela) ponto(monitor, x, y int) (int32, int32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	esc, ok := t.escala[monitor]
	if !ok {
		return 0, 0, errors.New("tire um print antes de usar o mouse: as coordenadas são as da imagem")
	}
	if monitor < 0 || monitor >= len(t.telas) {
		return 0, 0, fmt.Errorf("monitor %d não existe", monitor)
	}
	d := t.telas[monitor]
	return d.GetX() + int32(float64(x)*esc), d.GetY() + int32(float64(y)*esc), nil
}

const (
	mMover = 0
	mDesce = 1
	mSobe  = 2
	mRoda  = 3
)

var botoes = map[string]int32{"": 1, "esquerdo": 1, "direito": 2, "meio": 4}

func (t *Tela) mouse(mask, x, y int32, mods []pb.ControlKey) error {
	return t.s.Send(&pb.Message{Union: &pb.Message_MouseEvent{MouseEvent: &pb.MouseEvent{Mask: mask, X: x, Y: y, Modifiers: mods}}})
}

// Mover leva o ponteiro.
func (t *Tela) Mover(monitor, x, y int) error {
	px, py, err := t.ponto(monitor, x, y)
	if err != nil {
		return err
	}
	return t.mouse(mMover, px, py, nil)
}

// Clicar no ponto da imagem.
func (t *Tela) Clicar(monitor, x, y int, botao string, vezes int, mods []pb.ControlKey) error {
	b, ok := botoes[botao]
	if !ok {
		return fmt.Errorf("botão %q: use esquerdo, direito ou meio", botao)
	}
	px, py, err := t.ponto(monitor, x, y)
	if err != nil {
		return err
	}
	if err := t.mouse(mMover, px, py, mods); err != nil {
		return err
	}
	time.Sleep(60 * time.Millisecond)
	for i := 0; i < max(vezes, 1); i++ {
		if err := t.mouse(b<<3|mDesce, px, py, mods); err != nil {
			return err
		}
		time.Sleep(40 * time.Millisecond)
		if err := t.mouse(b<<3|mSobe, px, py, mods); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
	}
	return nil
}

// Arrastar de um ponto a outro.
func (t *Tela) Arrastar(monitor, x1, y1, x2, y2 int) error {
	ax, ay, err := t.ponto(monitor, x1, y1)
	if err != nil {
		return err
	}
	bx, by, _ := t.ponto(monitor, x2, y2)
	t.mouse(mMover, ax, ay, nil)
	time.Sleep(80 * time.Millisecond)
	if err := t.mouse(1<<3|mDesce, ax, ay, nil); err != nil {
		return err
	}
	const passos = 12
	for i := int32(1); i <= passos; i++ {
		time.Sleep(25 * time.Millisecond)
		t.mouse(mMover, ax+(bx-ax)*i/passos, ay+(by-ay)*i/passos, nil)
	}
	time.Sleep(80 * time.Millisecond)
	return t.mouse(1<<3|mSobe, bx, by, nil)
}

// Rolar a roda do mouse no ponto: dy positivo sobe, negativo desce.
func (t *Tela) Rolar(monitor, x, y, dx, dy int) error {
	if err := t.Mover(monitor, x, y); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return t.mouse(mRoda, int32(dx), int32(dy), nil)
}

func (t *Tela) tecla(k *pb.KeyEvent) error {
	k.Mode = pb.KeyboardMode_Legacy
	k.Press = true
	return t.s.Send(&pb.Message{Union: &pb.Message_KeyEvent{KeyEvent: k}})
}

// Digitar texto, com acento. Quebra de linha vira Enter.
func (t *Tela) Digitar(texto string) error {
	for i, linha := range strings.Split(strings.ReplaceAll(texto, "\r\n", "\n"), "\n") {
		if i > 0 {
			if err := t.tecla(&pb.KeyEvent{Union: &pb.KeyEvent_ControlKey{ControlKey: pb.ControlKey_Return}}); err != nil {
				return err
			}
			time.Sleep(60 * time.Millisecond)
		}
		for len(linha) > 0 {
			n := min(len(linha), 120)
			for n < len(linha) && !utf8.RuneStart(linha[n]) {
				n--
			}
			if err := t.tecla(&pb.KeyEvent{Union: &pb.KeyEvent_Seq{Seq: linha[:n]}}); err != nil {
				return err
			}
			linha = linha[n:]
			time.Sleep(40 * time.Millisecond)
		}
	}
	return nil
}

var teclas = map[string]pb.ControlKey{
	"enter": pb.ControlKey_Return, "tab": pb.ControlKey_Tab, "esc": pb.ControlKey_Escape, "backspace": pb.ControlKey_Backspace,
	"delete": pb.ControlKey_Delete, "del": pb.ControlKey_Delete, "insert": pb.ControlKey_Insert, "espaco": pb.ControlKey_Space,
	"cima": pb.ControlKey_UpArrow, "baixo": pb.ControlKey_DownArrow, "esquerda": pb.ControlKey_LeftArrow, "direita": pb.ControlKey_RightArrow,
	"home": pb.ControlKey_Home, "end": pb.ControlKey_End, "pageup": pb.ControlKey_PageUp, "pagedown": pb.ControlKey_PageDown,
	"capslock": pb.ControlKey_CapsLock, "win": pb.ControlKey_Meta, "menu": pb.ControlKey_Apps, "printscreen": pb.ControlKey_Snapshot,
	"f1": pb.ControlKey_F1, "f2": pb.ControlKey_F2, "f3": pb.ControlKey_F3, "f4": pb.ControlKey_F4, "f5": pb.ControlKey_F5, "f6": pb.ControlKey_F6,
	"f7": pb.ControlKey_F7, "f8": pb.ControlKey_F8, "f9": pb.ControlKey_F9, "f10": pb.ControlKey_F10, "f11": pb.ControlKey_F11, "f12": pb.ControlKey_F12,
	"ctrl+alt+del": pb.ControlKey_CtrlAltDel,
}

var modificadores = map[string]pb.ControlKey{
	"ctrl": pb.ControlKey_Control, "shift": pb.ControlKey_Shift, "alt": pb.ControlKey_Alt, "win": pb.ControlKey_Meta,
}

// Modificadores traduz ["ctrl","shift"].
func Modificadores(nomes []string) ([]pb.ControlKey, error) {
	var out []pb.ControlKey
	for _, n := range nomes {
		k, ok := modificadores[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return nil, fmt.Errorf("modificador %q: use ctrl, shift, alt ou win", n)
		}
		out = append(out, k)
	}
	return out, nil
}

// Teclas aperta uma combinação: "enter", "ctrl+s", "alt+f4", "win+r", "ctrl+alt+del".
func (t *Tela) Teclas(combo string) error {
	k, err := evento(combo)
	if err != nil {
		return err
	}
	return t.tecla(k)
}

func evento(combo string) (*pb.KeyEvent, error) {
	c := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(combo), " ", ""))
	if c == "" {
		return nil, errors.New("combinação vazia")
	}
	if c == "ctrl+alt+del" {
		return &pb.KeyEvent{Union: &pb.KeyEvent_ControlKey{ControlKey: pb.ControlKey_CtrlAltDel}}, nil
	}
	partes := strings.Split(c, "+")
	ultima := partes[len(partes)-1]
	mods, err := Modificadores(partes[:len(partes)-1])
	if err != nil {
		return nil, err
	}
	if k, ok := teclas[ultima]; ok {
		return &pb.KeyEvent{Union: &pb.KeyEvent_ControlKey{ControlKey: k}, Modifiers: mods}, nil
	}
	r, n := utf8.DecodeRuneInString(ultima)
	if n == 0 || n != len(ultima) {
		return nil, fmt.Errorf("tecla %q desconhecida", ultima)
	}
	return &pb.KeyEvent{Union: &pb.KeyEvent_Chr{Chr: uint32(r)}, Modifiers: mods}, nil
}

// Vivo diz se a sessão de tela está de pé.
func (t *Tela) Vivo() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.fim == nil }

// Fechar encerra a sessão de tela.
func (t *Tela) Fechar() { t.cancel(); t.s.Close("") }
