// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

func realEmail(v string) bool {
	at := strings.LastIndexByte(v, '@')
	if strings.EqualFold(v[:at], "git") {
		return false
	}
	host := strings.ToLower(v[at+1:])
	if !mailHost(v[:at], host) {
		return false
	}
	if fileHost(host) || docFiles[host] || !mailTLD(host[strings.LastIndexByte(host, '.')+1:]) {
		return false
	}
	for _, fake := range []string{"example.com", "example.org", "example.net", "localhost", ".test", ".invalid", ".example"} {
		if host == strings.TrimPrefix(fake, ".") || strings.HasSuffix(host, fake) {
			return false
		}
	}
	return !strings.HasPrefix(host, "noreply") && !strings.Contains(v, "noreply@")
}

// mailHost is whether host, after local and an @, can be a mail domain:
// it has a dot, labels of letters, digits and hyphens (or a name's own
// script's: 例子.中国), and is not a
// linker's table; and local has no @ of its own. A disassembly's symbols
// read as addresses: system@plt, puts@got.plt, a version after one @ or
// two (memcpy@GLIBC_2.14, foo@@GLIBC_2.2.5, foo@Base). .got is a
// top-level domain, and the local model took system@got for an email, so
// a reverse engineer had to turn masking off (ch35tnut_ on X, 10-28).
func mailHost(local, host string) bool {
	if strings.HasSuffix(local, "@") || !strings.Contains(host, ".") || strings.Trim(host, ".") != host {
		return false
	}
	linker := true
	for _, l := range strings.Split(host, ".") {
		if l == "" || strings.ContainsFunc(l, func(r rune) bool { return r < 0x80 && r != '-' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') }) {
			return false
		}
		if !linkerWords[l] {
			linker = false
		}
	}
	return !linker
}

// linkerWords are the names of a binary's tables a symbol is reached
// through (system@plt, puts@got.plt, foo@plt.sec), no domain's labels.
var linkerWords = map[string]bool{"plt": true, "got": true, "sec": true, "iplt": true, "igot": true}

// docFiles are the files agents read by name, on a domain's ending
// (.md): "@AGENTS.md" after a word, as a prompt names one, is no address
// (ydzh on Discord: xxx@Agents.md was masked).
var docFiles = map[string]bool{
	"agents.md": true, "claude.md": true, "gemini.md": true, "readme.md": true, "skill.md": true,
	"changelog.md": true, "contributing.md": true, "license.md": true, "lessons.md": true, "todo.md": true,
	"copilot-instructions.md": true, "crush.md": true, "qwen.md": true, "iflow.md": true, "warp.md": true,
}

// localTLDs are names an organisation's own mail can end in that no
// registry has: an address there is someone's all the same.
var localTLDs = map[string]bool{"local": true, "internal": true, "lan": true, "corp": true, "home": true, "intranet": true, "localdomain": true}

// mailTLD is whether tld, an address's last label, is one mail can be
// sent to: in the public suffix list's ICANN part, or an organisation's
// own. A decorator's or a module's attribute after a name
// (x@pytest.fixture, ydzh on Discord) ends in a word no registry has.
func mailTLD(tld string) bool {
	if localTLDs[tld] {
		return true
	}
	_, icann := publicsuffix.PublicSuffix(tld)
	return icann
}

// fileExt are file name extensions that are no top-level domain, so
// what ends in one is a file's name and not an address: an image a Mac
// saved at twice its size, 261020_14-16-11@2x.jpg, went to the model as
// {{EMAIL_…}} and Pi could no longer read the file (#3029).
// Other files are named so too (intro@2x.mp4, strings@base.json), and
// none of these is a domain either (IANA's list, checked 2026-10-21).
var fileExt = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "svg": true, "heic": true, "heif": true,
	"avif": true, "bmp": true, "tif": true, "tiff": true, "ico": true, "icns": true, "pdf": true,
	"svgz": true, "apng": true, "jxl": true, "psd": true, "eps": true, "lottie": true,
	"mp4": true, "m4v": true, "webm": true, "mkv": true, "avi": true, "mp3": true, "m4a": true, "wav": true,
	"ogg": true, "flac": true, "aac": true, "ttf": true, "otf": true, "woff": true, "woff2": true, "eot": true,
	"json": true, "yaml": true, "yml": true, "toml": true, "xml": true, "txt": true, "csv": true, "tsv": true,
	"log": true, "html": true, "htm": true, "css": true, "scss": true, "less": true, "js": true, "mjs": true,
	"cjs": true, "jsx": true, "ts": true, "tsx": true, "doc": true, "docx": true, "xls": true, "xlsx": true,
	"ppt": true, "pptx": true, "gz": true, "tgz": true, "tar": true, "rar": true, "7z": true, "dmg": true,
}

// scaleExt are file extensions that are top-level domains too (.zip,
// .mov, .md, .map): a name ending in one is a file's only with a scale
// before it, icon@2x.zip, intro@1.5x.mov, while ops@files.zip is an
// address.
var scaleExt = map[string]bool{"zip": true, "mov": true, "md": true, "map": true}

var scaleRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?x$`)

// fileHost says host, what follows the @ of what looks like an address,
// is the end of a file's name: an extension no domain has (fileExt), or a
// scale and an extension that is a domain too (scaleExt).
func fileHost(host string) bool {
	dot := strings.LastIndexByte(host, '.')
	ext := host[dot+1:]
	return fileExt[ext] || dot > 0 && scaleExt[ext] && scaleRe.MatchString(host[:dot])
}

func chineseID(v string) bool {
	w := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(v[i]-'0') * w[i]
	}
	return strings.ToUpper(v[17:]) == string("10X98765432"[sum%11])
}

func luhn(v string) bool {
	var ds []int
	for _, r := range v {
		if r >= '0' && r <= '9' {
			ds = append(ds, int(r-'0'))
		}
	}
	if len(ds) < 15 || len(ds) > 19 {
		return false
	}
	sum := 0
	for i := range ds {
		d := ds[len(ds)-1-i]
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
