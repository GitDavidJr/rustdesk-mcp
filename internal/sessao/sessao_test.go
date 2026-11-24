package sessao

import (
	"encoding/base64"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
)

// O console do Windows ecoa o comando, quebra a linha na largura e enfia sequências de tela no meio.
func TestExtrairIgnoraEcoPromptEQuebra(t *testing.T) {
	saida := "Versão 5 · ação\nlinha 2 <<RDF:outro:9>>"
	b := base64.StdEncoding.EncodeToString([]byte(saida))
	bruto := "\x1b[?25l\x1b[HPS C:\\Users\\Cliente> powershell -EncodedCommand AAAA\x1b[K\r\n" +
		"<<RDI:abc123:0>>\r\n\x1b[5;1H" + b[:10] + "\r\n\x1b[K" + b[10:] + "\x1b[m\r\n<<RDF:abc123:3>>\r\nPS C:\\>"
	r, ok, err := extrair(bruto, "abc123")
	if !ok || err != nil || r.Codigo != 3 || r.Saida != saida || r.Cortada {
		t.Fatalf("%+v ok=%v err=%v", r, ok, err)
	}
	if _, ok, _ := extrair(bruto[:len(bruto)-20], "abc123"); ok {
		t.Fatal("deu por terminado sem a marca final")
	}
	if _, ok, _ := extrair(bruto, "zzz"); ok {
		t.Fatal("aceitou a marca de outro comando")
	}
}

func TestExtrairSaidaCorrompidaAvisa(t *testing.T) {
	_, ok, err := extrair("<<RDI:a:0>>\nQUJ\n<<RDF:a:0>>", "a")
	if !ok || err == nil {
		t.Fatal("base64 partido precisa virar erro, não saída errada")
	}
}

// A marca não pode aparecer no eco do comando: só a resposta a contém.
func TestComandoNaoContemAMarca(t *testing.T) {
	for _, l := range []string{linhaPowerShell("id9", "Get-Date", `C:\Pasta d'água`, 100), linhaSh("id9", "date", "/tmp", 100)} {
		if strings.Contains(l, "<<RDI:id9") || strings.Contains(l, "<<RDF:id9") {
			t.Fatal("a marca viaja em claro no comando: ", l)
		}
	}
}

func TestComandoLongoNaoVaiNaLinha(t *testing.T) {
	if l := linhaPowerShell("x", strings.Repeat("Get-Date;", 600), "", 100); len(l) <= limiteLinha {
		t.Fatal("o limite de linha não seria respeitado")
	}
}

func TestLimpar(t *testing.T) {
	if s := Limpar("\x1b]0;título\x07\x1b[31mok\x1b[0m\r\nação\x08!"); s != "ok\nação!" {
		t.Fatalf("%q", s)
	}
}

func telaTeste() *Tela {
	return &Tela{escala: map[int]float64{}, telas: []*pb.DisplayInfo{{X: 0, Y: 0, Width: 1920, Height: 1080}, {X: -1280, Y: 40, Width: 1280, Height: 720}}}
}

func TestCoordenadasSeguemAUltimaImagemInteira(t *testing.T) {
	tl := telaTeste()
	if _, _, err := tl.ponto(0, 1, 1); err == nil {
		t.Fatal("clicar sem print tem de falhar")
	}
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	img.Set(1900, 1000, color.White)
	c, err := tl.montar(img, 0, 960, nil, 0)
	if err != nil || c.Largura != 960 || c.Altura != 540 || c.Escala != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if x, y, _ := tl.ponto(0, 480, 270); x != 960 || y != 540 {
		t.Fatal(x, y)
	}
	r, err := tl.montar(img, 0, 960, &Recorte{X: 800, Y: 400, Largura: 100, Altura: 100}, 2)
	if err != nil || r.Largura != 200 || r.Altura != 200 {
		t.Fatalf("recorte deve vir em resolução cheia: %+v %v", r, err)
	}
	if x, _, _ := tl.ponto(0, 480, 270); x != 960 {
		t.Fatal("o recorte mudou o sistema de coordenadas dos cliques")
	}
	if _, err := tl.montar(img, 0, 960, &Recorte{X: 5000, Y: 5000, Largura: 10, Altura: 10}, 2); err == nil {
		t.Fatal("recorte fora da imagem")
	}
	img2 := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	if _, err := tl.montar(img2, 1, 1568, nil, 0); err != nil {
		t.Fatal(err)
	}
	if x, y, _ := tl.ponto(1, 10, 10); x != -1270 || y != 50 {
		t.Fatal("monitor à esquerda tem origem negativa", x, y)
	}
}

func TestTeclas(t *testing.T) {
	k, err := evento("Ctrl + Shift+ESC")
	if err != nil || k.GetControlKey() != pb.ControlKey_Escape || len(k.Modifiers) != 2 {
		t.Fatal(k, err)
	}
	if k, _ := evento("win+r"); k.GetChr() != 'r' || k.Modifiers[0] != pb.ControlKey_Meta {
		t.Fatal(k)
	}
	if k, _ := evento("ctrl+alt+del"); k.GetControlKey() != pb.ControlKey_CtrlAltDel {
		t.Fatal(k)
	}
	for _, ruim := range []string{"", "ctrl+", "hiper+x", "naoexiste"} {
		if _, err := evento(ruim); err == nil {
			t.Fatal("aceitou", ruim)
		}
	}
}
