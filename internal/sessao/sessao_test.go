package sessao

import (
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"os/exec"
	"strings"
	"testing"

	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
)

func resposta(id, saida string, codigo int) string {
	b := base64.StdEncoding.EncodeToString([]byte(saida))
	var sb strings.Builder
	n := 0
	fmt.Fprintf(&sb, "<<RDI:%s:00>>\r\n", id)
	for i := 0; i < len(b); i += 160 {
		fmt.Fprintf(&sb, "~%d~%s~\r\n", n, b[i:min(i+160, len(b))])
		n++
	}
	fmt.Fprintf(&sb, "<<RDF:%s:%d:%d>>\r\n", id, codigo, n)
	return sb.String()
}

// O console do Windows ecoa o comando, quebra a linha na largura e enfia sequências de tela no meio.
func TestExtrairIgnoraEcoPromptEQuebra(t *testing.T) {
	saida := strings.Repeat("Versão 5 · ação\n", 60) + "fim <<RDF:outro:9:1>>"
	bruto := resposta("abc123", saida, 3)
	bruto = strings.Replace(bruto, "~1~", "\x1b[5;1H~1\x1b[K~", 1)
	bruto = strings.Replace(bruto, "~2~", "~2~\r\n\x1b[m", 1) // quebrado pela largura
	bruto = "\x1b[?25l\x1b[H> __rd 'abc123' 'AAAA' 100\x1b[K\r\n" + bruto + "> "
	r, ok, err := extrair(bruto, "abc123")
	if !ok || err != nil || r.Codigo != 3 || r.Saida != saida || r.Cortada {
		t.Fatalf("ok=%v err=%v codigo=%d igual=%v", ok, err, r.Codigo, r.Saida == saida)
	}
	if _, ok, _ := extrair(bruto[:len(bruto)-20], "abc123"); ok {
		t.Fatal("deu por terminado sem a marca final")
	}
	if _, ok, _ := extrair(bruto, "zzz"); ok {
		t.Fatal("aceitou a marca de outro comando")
	}
}

// Repintura repete linha (aceita); linha que sumiu vira erro, nunca saída errada.
func TestExtrairRepinturaEFalta(t *testing.T) {
	saida := strings.Repeat("x", 600)
	bruto := resposta("a", saida, 0)
	linhas := strings.Split(bruto, "\r\n")
	repetida := strings.Join(append(append([]string{}, linhas[:3]...), linhas[1:]...), "\r\n")
	if r, ok, err := extrair(repetida, "a"); !ok || err != nil || r.Saida != saida {
		t.Fatal(ok, err, len(r.Saida))
	}
	falta := strings.Join(append(append([]string{}, linhas[:2]...), linhas[3:]...), "\r\n")
	if _, ok, err := extrair(falta, "a"); !ok || err == nil {
		t.Fatal("pedaço perdido precisa virar erro")
	}
}

func TestExtrairSaidaVaziaEArquivo(t *testing.T) {
	if r, ok, err := extrair(resposta("a", "", 0), "a"); !ok || err != nil || r.Saida != "" {
		t.Fatal(r, ok, err)
	}
	arq := strings.Replace(resposta("a", `C:\Temp\rdmcp-a.txt`, 0), ":00>>", ":11>>", 1)
	if r, _, err := extrair(arq, "a"); err != nil || r.Arquivo != `C:\Temp\rdmcp-a.txt` || !r.Cortada || r.Saida != "" {
		t.Fatal(r, err)
	}
}

// A marca não pode aparecer no eco do comando nem na função auxiliar: só a resposta a contém.
func TestComandoNaoContemAMarca(t *testing.T) {
	for _, l := range []string{linhaPowerShell("id9", "Get-Date", `C:\Pasta d'água`, 100, false), linhaSh("id9", "date", "/tmp", 100), auxiliar} {
		if strings.Contains(l, "<<RD") {
			t.Fatal("a marca viaja em claro no comando: ", l)
		}
	}
	if strings.ContainsAny(auxiliar, "\n\t") {
		t.Fatal("a função auxiliar precisa caber numa linha, sem tabulação (o console completa nomes)")
	}
}

func TestComandoLongoNaoVaiNaLinha(t *testing.T) {
	if l := linhaPowerShell("x", strings.Repeat("Get-Date;", 600), "", 100, true); len(l) <= limiteLinha {
		t.Fatal("o limite de linha não seria respeitado")
	}
}

// O caminho de Linux/macOS roda de verdade aqui.
func TestLinhaShDeVerdade(t *testing.T) {
	for _, c := range []struct {
		cmd, quer string
		cod       int
	}{{"echo ação; echo dois", "ação\ndois", 0}, {"exit 4", "", 4}, {"ls /naoexiste", "", -1}, {"seq 1 400", "", 0}} {
		b, _ := exec.Command("sh", "-c", linhaSh("abc", c.cmd, "/tmp", 30000)).CombinedOutput()
		r, ok, err := extrair(string(b), "abc")
		if !ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v %q", c.cmd, ok, err, b)
		}
		if c.cod >= 0 && r.Codigo != c.cod || c.cod < 0 && r.Codigo == 0 {
			t.Fatalf("%s: código %d", c.cmd, r.Codigo)
		}
		if c.quer != "" && r.Saida != c.quer {
			t.Fatalf("%s: %q", c.cmd, r.Saida)
		}
		if c.cmd == "seq 1 400" && !strings.HasSuffix(r.Saida, "\n399\n400") {
			t.Fatalf("saída longa: %q", r.Saida[max(0, len(r.Saida)-20):])
		}
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
