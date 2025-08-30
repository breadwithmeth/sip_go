package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

// ------------------ ПАРАМЕТРЫ ------------------
const (
	// Пример настроек SIP — подставь свои
	sipServer = "91.244.101.82"
	sipAddr   = "91.244.101.82:5060"
	username  = "136"
	password  = "RnJLOEt1MnFvNTg9"

	// номер укажи через -number
	dstNumber    = ""
	playbackFile = "prompt.alaw"
	recordFile   = "recorded.alaw"

	expiration = 300
	userAgent  = "go-min-sip/0.1"
)

// общий быстрый HTTP клиент
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		DisableCompression:    false,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
}

// ============================================================================
//                         AI: генерация RU/KK текстов
// ============================================================================

type infoTexts struct {
	RU string `json:"ru"`
	KK string `json:"kk"`
}

// generateInfoTexts — просим OpenAI сделать короткие, вежливые тексты на RU/KK.
// Возвращаем оба. Требуется OPENAI_API_KEY.
func generateInfoTexts(rawInfo string) (infoTexts, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return infoTexts{}, fmt.Errorf("OPENAI_API_KEY not set")
	}

	sys := "You are a helpful medical call assistant. Output strictly JSON with keys 'ru' and 'kk'. No extra text."
	user := fmt.Sprintf(`Сформулируй короткое, вежливое уведомление для пациента о необходимости визита/обследования на двух языках.
Требования:
- Лаконично: 1 короткое предложение на язык.
- Без обращения по имени.
- Без советов и диагнозов.
- Русский ключ: "ru", казахский ключ: "kk".
- Ответ СТРОГО в виде JSON без пояснений.

Смысл сообщения: %s`, strings.TrimSpace(rawInfo))

	reqBody := map[string]any{
		"model": "gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
		"temperature":     0.2,
		"response_format": map[string]string{"type": "json_object"},
	}
	jb, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jb))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return infoTexts{}, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return infoTexts{}, fmt.Errorf("OpenAI chat %d: %s", resp.StatusCode, truncate(string(b), 400))
	}

	var c struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return infoTexts{}, err
	}
	if len(c.Choices) == 0 {
		return infoTexts{}, fmt.Errorf("no choices from OpenAI")
	}
	raw := strings.TrimSpace(c.Choices[0].Message.Content)

	var out infoTexts
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// fallback: если модель вернула не JSON
		out.RU = fmt.Sprintf("Вам нужно прийти на обследование. %s.", rawInfo)
		out.KK = fmt.Sprintf("Сізге тексеруден өту қажет. %s.", rawInfo)
	}
	if strings.TrimSpace(out.RU) == "" {
		out.RU = fmt.Sprintf("Вам нужно прийти на обследование. %s.", rawInfo)
	}
	if strings.TrimSpace(out.KK) == "" {
		out.KK = fmt.Sprintf("Сізге тексеруден өту қажет. %s.", rawInfo)
	}
	return out, nil
}

// ============================================================================
//                            Patient-info и фразы
// ============================================================================

type PatientInfo struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	PhoneNumber string     `json:"phone_number"`
	CallAt      *time.Time `json:"call_at"`
}
type PatientInfoResponse struct {
	Patient     PatientInfo `json:"patient"`
	Information string      `json:"information"`
}

// тянем /v1/voice/patient-info?phone_number=...
func fetchPatientInfo(phone string) (PatientInfoResponse, error) {
	base := getAPIBase()
	u := base + "/patient-info?phone_number=" + url.QueryEscape(phone)

	req, _ := http.NewRequest("GET", u, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return PatientInfoResponse{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return PatientInfoResponse{}, fmt.Errorf("patient-info %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var out PatientInfoResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return PatientInfoResponse{}, err
	}
	return out, nil
}

type DialoguePack struct {
	AskLangRU_ALaw string // "Выберите язык..."
	AskLangKK_ALaw string // "Қай тілде сөйлесеміз..."
	ConfirmNameRU  string // alaw
	ConfirmNameKK  string // alaw
	InfoRU         string // alaw
	InfoKK         string // alaw
	AskCallbackRU  string // alaw
	AskCallbackKK  string // alaw
}

// детектор языка (простые триггеры)
func detectLang(s string) string {
	t := strings.ToLower(s)
	kkTokens := []string{
		"қаз", "қазақ", "қазақша", "kk", "kz",
		"иә", "ия", "иа", "ия.", "ия,", "ия!", "жарайды",
	}
	for _, w := range kkTokens {
		if strings.Contains(t, w) {
			return "kk"
		}
	}
	ruTokens := []string{"рус", "по-рус", "на рус", "ru", "russian"}
	for _, w := range ruTokens {
		if strings.Contains(t, w) {
			return "ru"
		}
	}
	return "ru"
}

func isAffirmative(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	yes := []string{"да", "ага", "угу", "ок", "хорошо", "конечно", "yes", "ok", "sure", "иә", "ия", "болад", "болады"}
	for _, w := range yes {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// генерим TTS обеих языков заранее и конвертируем в A-law RAW
func pregenPrompts(baseName, patientName, info string) (DialoguePack, error) {
	askLangRU := "Здравствуйте! Выберите язык: русский или казахский."
	askLangKK := "Сәлеметсіз бе! Қай тілде сөйлесеміз: қазақша ма, әлде орысша ма?"
	confirmRU := fmt.Sprintf("Вас зовут %s?", patientName)
	confirmKK := fmt.Sprintf("Сіздің атыңыз %s ме?", patientName)

	// Получаем информационные фразы от модели (RU/KK)
	aiTexts, err := generateInfoTexts(info)
	if err != nil {
		aiTexts = infoTexts{
			RU: fmt.Sprintf("Вам нужно прийти на обследование. %s.", info),
			KK: fmt.Sprintf("Сізге тексеруден өту қажет. %s.", info),
		}
	}

	cbRU := "Когда можно перезвонить, чтобы услышать пациента лично?"
	cbKK := "Қашан қайта қоңырау шалуға болады, науқастың өзімен сөйлесу үшін?"

	// helper: tts -> 8k wav -> alaw
	makeALaw := func(text, stem string) (string, error) {
		wav := stem + ".wav"
		if err := GenerateOpenAITTS(text, wav); err != nil {
			return "", err
		}
		norm := stem + "_8k_mono.wav"
		if err := ensureWavPCM16Mono8kNoFFMPEG(wav, norm); err != nil {
			return "", err
		}
		alaw := stem + ".alaw"
		if err := wavToALawRaw(norm, alaw); err != nil {
			return "", err
		}
		return alaw, nil
	}

	var p DialoguePack

	if p.AskLangRU_ALaw, err = makeALaw(askLangRU, baseName+"_ask_lang_ru"); err != nil {
		return p, fmt.Errorf("ask_lang_ru: %w", err)
	}
	if p.AskLangKK_ALaw, err = makeALaw(askLangKK, baseName+"_ask_lang_kk"); err != nil {
		return p, fmt.Errorf("ask_lang_kk: %w", err)
	}

	if p.ConfirmNameRU, err = makeALaw(confirmRU, baseName+"_confirm_ru"); err != nil {
		return p, err
	}
	if p.ConfirmNameKK, err = makeALaw(confirmKK, baseName+"_confirm_kk"); err != nil {
		return p, err
	}

	if p.InfoRU, err = makeALaw(aiTexts.RU, baseName+"_info_ru"); err != nil {
		return p, err
	}
	if p.InfoKK, err = makeALaw(aiTexts.KK, baseName+"_info_kk"); err != nil {
		return p, err
	}

	if p.AskCallbackRU, err = makeALaw(cbRU, baseName+"_cb_ru"); err != nil {
		return p, err
	}
	if p.AskCallbackKK, err = makeALaw(cbKK, baseName+"_cb_kk"); err != nil {
		return p, err
	}

	return p, nil
}

// Храним текущие подготовленные реплики и выбранный язык
var currentPack DialoguePack
var chosenLang = "ru"

// ============================================================================
//                               main / runCall
// ============================================================================

func main() {
	number := flag.String("number", dstNumber, "destination phone number")
	flag.Parse()
	if number == nil || *number == "" {
		log.Fatal("provide -number <PHONE>")
	}
	if err := runCall(*number); err != nil {
		log.Fatalf("call failed: %v", err)
	}
}

// runCall — всё взаимодействие (SIP + RTP + диалог)

func runCall(number string) error {
	// Стартовая реплика (вопрос о языке) → в playbackFile
	if err := startVoiceSession(number); err != nil {
		return fmt.Errorf("startVoiceSession: %w", err)
	}

	raddr, err := net.ResolveUDPAddr("udp", sipAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	localIP := localAddr.IP.String()
	localPort := localAddr.Port

	log.Printf("Local SIP socket %s:%d", localIP, localPort)

	callID := buildCallID()
	fromTag := buildTag()

	// REGISTER
	cseq := 1
	reg := buildRegister(localIP, localPort, callID, fromTag, cseq, nil)
	if err := send(conn, reg); err != nil {
		return fmt.Errorf("send REGISTER: %w", err)
	}
	log.Println("REGISTER sent (no auth)")

	var authReg *digestAuth
	for {
		pkt, err := readPacket(conn)
		if err != nil {
			return fmt.Errorf("read REGISTER resp: %w", err)
		}
		code := parseStatusCode(pkt)
		log.Printf("REGISTER resp code=%d\n%s", code, firstLines(pkt, 4))
		if code == 401 {
			realm := extract(pkt, reRealm)
			nonce := extract(pkt, reNonce)
			if realm == "" || nonce == "" {
				return fmt.Errorf("no realm/nonce in 401")
			}
			authReg = &digestAuth{realm: realm, nonce: nonce}
			cseq++
			regAuth := buildRegister(localIP, localPort, callID, fromTag, cseq, authReg)
			if err := send(conn, regAuth); err != nil {
				return fmt.Errorf("send REGISTER auth: %w", err)
			}
			log.Println("REGISTER sent (auth)")
			continue
		}
		if code == 200 {
			log.Println("REGISTER OK")
			break
		}
		if code >= 300 {
			return fmt.Errorf("REGISTER failed code=%d", code)
		}
	}

	// INVITE
	inviteCSeq := cseq + 1
	mediaPort := 40000 + r.Intn(10000)
	rtpListenAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("0.0.0.0:%d", mediaPort))
	if err != nil {
		return fmt.Errorf("resolve RTP addr: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp", rtpListenAddr)
	if err != nil {
		return fmt.Errorf("listen RTP: %w", err)
	}
	defer rtpConn.Close()
	log.Printf("RTP listening on %s", rtpConn.LocalAddr())

	invite := buildInvite(localIP, localPort, mediaPort, callID, fromTag, number, inviteCSeq, authReg)
	if err := send(conn, invite); err != nil {
		return fmt.Errorf("send INVITE: %w", err)
	}
	log.Println("INVITE sent")

	var toTag string
	var remoteMediaIP string
	var remoteMediaPort int
	answered := false
	byeCSeq := inviteCSeq + 1
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && !answered {
		pkt, err := readPacket(conn)
		if err != nil {
			log.Println("read INVITE resp:", err)
			continue
		}
		parts := strings.Split(pkt, "\r\n\r\n")
		headers := parts[0]
		body := ""
		if len(parts) > 1 {
			body = parts[1]
		}
		code := parseStatusCode(pkt)
		short := firstLines(pkt, 8)
		log.Printf("INVITE resp code=%d\n%s", code, short)
		if code == 401 || code == 407 {
			realm := extract(pkt, reRealm)
			nonce := extract(pkt, reNonce)
			if realm != "" && nonce != "" {
				authReg = &digestAuth{realm: realm, nonce: nonce}
				inviteCSeq++
				invite = buildInvite(localIP, localPort, mediaPort, callID, fromTag, number, inviteCSeq, authReg)
				if err := send(conn, invite); err != nil {
					return fmt.Errorf("reINVITE auth send: %w", err)
				}
				log.Println("Re-INVITE sent (auth)")
			}
		} else if code == 180 || code == 183 {
			continue
		} else if code == 200 {
			scanner := bufio.NewScanner(strings.NewReader(headers))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(strings.ToLower(line), "to:") && strings.Contains(strings.ToLower(line), ";tag=") {
					toTag = extract(line+"\r\n", reToTag)
					break
				}
			}
			if toTag == "" {
				log.Println("WARN: no to-tag in 200 OK")
			}
			remoteMediaIP, remoteMediaPort = parseSDP(body, sipServer)
			ack := buildAck(localIP, localPort, callID, fromTag, toTag, number, inviteCSeq)
			if err := send(conn, ack); err != nil {
				return fmt.Errorf("send ACK: %w", err)
			}
			log.Printf("ACK sent. Remote media %s:%d", remoteMediaIP, remoteMediaPort)
			answered = true
		} else if code >= 300 {
			return fmt.Errorf("Call failed code=%d", code)
		}
	}

	if !answered {
		return fmt.Errorf("call not answered / not established within timeout")
	}

	if remoteMediaPort == 0 {
		log.Println("No remote media port; skip RTP phase")
		return nil
	}

	remoteAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", remoteMediaIP, remoteMediaPort))
	if err != nil {
		log.Println("resolve remote RTP:", err)
		return nil
	}

	// Быстрая проверка RTP
	{
		rtpBuf := make([]byte, 1500)
		gotRTP := false
		rtpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for !gotRTP {
			n, _, err := rtpConn.ReadFromUDP(rtpBuf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					break
				}
				log.Println("RTP read error:", err)
				break
			}
			if n >= 12 {
				gotRTP = true
				break
			}
		}
		if !gotRTP {
			log.Println("Нет RTP пакетов от клиента; завершение и рестарт…")
			return runCall(number)
		}
	}

	// ---------- ФАЗА 1: выбор языка (повторять, пока не ru/kk) ----------
	chosenLang := ""
	for attempts := 1; attempts <= 100 && chosenLang == ""; attempts++ {
		log.Printf("[LangSelect] Attempt %d", attempts)

		// play
		if _, err := os.Stat(playbackFile); err != nil {
			log.Printf("No playback file %s: %v", playbackFile, err)
		} else if err := rtpPlayOnConn(rtpConn, remoteAddr, playbackFile); err != nil {
			log.Println("rtpPlay error:", err)
		}

		// record 3s
		if err := rtpRecordFixedDuration(rtpConn, recordFile, 1*time.Second); err != nil {
			log.Println("recording error:", err)
			continue
		}
		// A-law RAW → WAV
		if err := alawRawToPCM16Wav(recordFile, "recorded.wav", 8000); err != nil {
			log.Println("alawRawToPCM16Wav error:", err)
			continue
		}
		// STT (без подсказки)
		raw, err := transcribeOpenAI("recorded.wav", chosenLang) // на первом круге ""
		if err != nil {
			log.Println("STT error:", err)
			continue
		}
		txt := strings.TrimSpace(raw)
		if err != nil {
			log.Println("STT error:", err)
			continue
		}
		tl := strings.ToLower(strings.TrimSpace(txt))
		log.Printf("[LangSelect] STT: %q", tl)

		switch {
		// kk-маркеры
		case strings.Contains(tl, "қаз"),
			strings.Contains(tl, "қазақ"),
			strings.Contains(tl, "қазақша"),
			strings.Contains(tl, "kk"),
			strings.Contains(tl, "kz"),
			strings.Contains(tl, "казах"):
			chosenLang = "kk"
		// ru-маркеры
		case strings.Contains(tl, "рус"),
			strings.Contains(tl, "ru"),
			strings.Contains(tl, "russian"),
			strings.Contains(tl, "по-рус"):
			chosenLang = "ru"
		default:
			chosenLang = ""
		}

		if chosenLang == "" {
			log.Println("[LangSelect] Language not detected; re-asking…")
			// (опционально) можно заменить playbackFile на дополнительный уточняющий вопрос
		}
	}
	if chosenLang == "" {
		log.Println("[LangSelect] language not chosen; hangup")
		return nil
	}
	log.Printf("[LangSelect] chosenLang=%s", chosenLang)

	// Подготовим prompt для подтверждения имени
	{
		next := map[string]string{
			"ru": currentPack.ConfirmNameRU,
			"kk": currentPack.ConfirmNameKK,
		}[chosenLang]
		if next == "" {
			next = currentPack.ConfirmNameRU
		}
		if b, err := os.ReadFile(next); err == nil {
			_ = os.WriteFile(playbackFile, b, 0644)
		}
	}

	// ---------- ФАЗА 2: подтверждение личности (повторять, пока не ДА/НЕТ) ----------
	confirmed := false
	denied := false
	for attempts := 1; attempts <= 100 && !confirmed && !denied; attempts++ {
		log.Printf("[ConfirmName] Attempt %d", attempts)

		// play
		if _, err := os.Stat(playbackFile); err != nil {
			log.Printf("No playback file %s: %v", playbackFile, err)
		} else if err := rtpPlayOnConn(rtpConn, remoteAddr, playbackFile); err != nil {
			log.Println("rtpPlay error:", err)
		}

		// record 3s
		if err := rtpRecordFixedDuration(rtpConn, recordFile, 1*time.Second); err != nil {
			log.Println("recording error:", err)
			continue
		}
		// A-law RAW → WAV
		if err := alawRawToPCM16Wav(recordFile, "recorded.wav", 8000); err != nil {
			log.Println("alawRawToPCM16Wav error:", err)
			continue
		}
		// STT с подсказкой языка (если есть версия с lang — подайте chosenLang)
		raw, err := transcribeOpenAI("recorded.wav", chosenLang) // на первом круге ""
		if err != nil {
			log.Println("STT error:", err)
			continue
		}
		txt := strings.TrimSpace(raw)
		log.Printf("[ConfirmName] STT: %q", txt)

		if isAffirmative(txt) {
			confirmed = true
			break
		}
		if isNegative(txt) {
			denied = true
			break
		}

		log.Println("[ConfirmName] Neither yes nor no; re-asking…")
		// (опционально) можно подменять playbackFile на более короткий/чёткий вариант вопроса
	}

	if !confirmed && !denied {
		log.Println("[ConfirmName] no decision; hangup")
		goto BYE
	}

	// ---------- ФИНАЛЬНАЯ ФАЗА ----------
	if confirmed {
		// 1) Сообщаем информацию и завершаем
		next := map[string]string{
			"ru": currentPack.InfoRU, // «Вам нужно прийти на обследование…»
			"kk": currentPack.InfoKK, // «Сізге тексеруден өту қажет…»
		}[chosenLang]
		if next == "" {
			next = currentPack.InfoRU
		}
		if b, err := os.ReadFile(next); err == nil {
			_ = os.WriteFile(playbackFile, b, 0644)
		}

		// play info
		if _, err := os.Stat(playbackFile); err == nil {
			_ = rtpPlayOnConn(rtpConn, remoteAddr, playbackFile)
		}
		// Тут можно не ждать ответа и сразу завершать
		goto BYE
	}

	if denied {
		// 2) Не тот человек → цикл "когда перезвонить?"
		// Сгенерируем TTS с именем пациента, чтобы обращение было персональным.
		patientName := os.Getenv("PATIENT_NAME") // или подставьте, если у вас есть из API
		if patientName == "" {
			patientName = "пациент"
		}

		var askText string
		if chosenLang == "kk" {
			askText = fmt.Sprintf("Қай уақытта қайта қоңырау шалуға болады, %s телефонды алатын кезде? Мысалы: ертең 15:30.", patientName)
		} else {
			askText = fmt.Sprintf("Когда удобно перезвонить, чтобы застать %s? Например: завтра в 15:30.", patientName)
		}

		// На каждой итерации спрашиваем → пишем 3с → STT → парсим → если не получилось, повторяем
		loc, _ := time.LoadLocation("Asia/Almaty")
		if loc == nil {
			loc = time.Local
		}

		for attempt := 1; attempt <= 20; attempt++ {
			// сгенерировать вопрос TTS на выбранном языке (динамический текст с именем)
			const ttsWav = "ask_callback.wav"
			if err := GenerateOpenAITTS(askText, ttsWav); err == nil {
				norm := "ask_callback_8k_mono.wav"
				if err := ensureWavPCM16Mono8kNoFFMPEG(ttsWav, norm); err == nil {
					_ = wavToALawRaw(norm, playbackFile)
				}
			} else {
				// fallback: статический файл из currentPack
				next := map[string]string{
					"ru": currentPack.AskCallbackRU,
					"kk": currentPack.AskCallbackKK,
				}[chosenLang]
				if next != "" {
					if b, err := os.ReadFile(next); err == nil {
						_ = os.WriteFile(playbackFile, b, 0644)
					}
				}
			}

			// play вопрос
			if _, err := os.Stat(playbackFile); err == nil {
				_ = rtpPlayOnConn(rtpConn, remoteAddr, playbackFile)
			}

			// запись ответа 3 секунды
			if err := rtpRecordFixedDuration(rtpConn, recordFile, 3*time.Second); err != nil {
				log.Println("recording error:", err)
				continue
			}
			// конверт
			if err := alawRawToPCM16Wav(recordFile, "recorded.wav", 8000); err != nil {
				log.Println("alawRawToPCM16Wav error:", err)
				continue
			}
			// STT (лучше с подсказкой языка, если у вас есть версия transcribeOpenAI(path, lang))
			raw, err := transcribeOpenAI("recorded.wav", chosenLang) // на первом круге ""
			if err != nil {
				log.Println("STT error:", err)
				continue
			}
			txt := strings.TrimSpace(raw)
			log.Printf("[AskCallback] STT: %q", txt)

			when, ok := parseCallbackTimeRU_KK(txt, loc)
			if !ok {
				// повторяем вопрос
				continue
			}

			// отправим на бэкенд (если есть API)
			if err := sendCallbackTime(number, when); err != nil {
				log.Println("sendCallbackTime:", err)
			}

			// Подтверждение времени TTS и завершение
			var confirm string
			// красивая фраза с датой/временем
			dowRu := [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
			dowKk := [...]string{"жексенбі", "дүйсенбі", "сейсенбі", "сәрсенбі", "бейсенбі", "жұма", "сенбі"}

			if chosenLang == "kk" {
				confirm = fmt.Sprintf("Жақсы, %s күні сағат %s қайта қоңырау шаламыз. Рақмет!", dowKk[int(when.Weekday())],
					when.Format("15:04"))
			} else {
				confirm = fmt.Sprintf("Хорошо, перезвоним в %s в %s. Спасибо!", dowRu[int(when.Weekday())],
					when.Format("15:04"))
			}

			const confWav = "confirm_callback.wav"
			if err := GenerateOpenAITTS(confirm, confWav); err == nil {
				norm := "confirm_callback_8k_mono.wav"
				if err := ensureWavPCM16Mono8kNoFFMPEG(confWav, norm); err == nil {
					_ = wavToALawRaw(norm, playbackFile)
				}
				if _, err := os.Stat(playbackFile); err == nil {
					_ = rtpPlayOnConn(rtpConn, remoteAddr, playbackFile)
				}
			}
			// после подтверждения — завершаем вызов
			return nil
		}

		// если 20 попыток не помогли — мягко завершаем
		return nil
	}

BYE:
	bye := buildBye(localIP, localPort, callID, fromTag, toTag, number, byeCSeq)
	if err := send(conn, bye); err != nil {
		return fmt.Errorf("send BYE: %w", err)
	}
	log.Println("BYE sent")
	if pkt, err := readPacket(conn); err == nil {
		log.Printf("BYE resp: %d", parseStatusCode(pkt))
	}
	return nil
}

func isNegative(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	neg := []string{
		"нет", "неа", "не я", "это не я", "no", "nope",
		"жоқ", "жок", "жооқ", "емес",
	}
	for _, w := range neg {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
func sendCallbackTime(phone string, at time.Time) error {
	base := getAPIBase() // уже есть у вас
	u := base + "/callback_time"
	vals := url.Values{}
	vals.Set("phone_number", phone)
	vals.Set("time", at.Format("2006-01-02 15:04")) // сервер пусть ждёт локальное время
	req, _ := http.NewRequest("POST", u, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("callback_time %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
func parseCallbackTimeRU_KK(text string, loc *time.Location) (time.Time, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	now := time.Now().In(loc)

	// День
	day := now
	switch {
	case strings.Contains(t, "завтра") || strings.Contains(t, "ертең"):
		day = now.AddDate(0, 0, 1)
	case strings.Contains(t, "послезавтра") || strings.Contains(t, "бүрсікүні"):
		day = now.AddDate(0, 0, 2)
	case strings.Contains(t, "сегодня") || strings.Contains(t, "бүгін"):
		day = now
	default:
		// оставляем как today (ниже если прошло – сдвинем)
	}

	// Время HH:MM или HH.MM или "сағат HH:MM"
	// ловим первую пару цифр
	re := regexp.MustCompile(`(?i)(\d{1,2})[:\.](\d{2})`)
	m := re.FindStringSubmatch(t)
	if len(m) < 3 {
		// попробуем "в HH" / "сағат HH"
		reH := regexp.MustCompile(`(?i)(?:в|сағат)\s*(\d{1,2})\b`)
		mh := reH.FindStringSubmatch(t)
		if len(mh) == 2 {
			h, _ := strconv.Atoi(mh[1])
			if h >= 0 && h <= 23 {
				proposed := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
				// если день не указан явно и время прошло — на завтра
				if !(strings.Contains(t, "завтра") || strings.Contains(t, "ертең") ||
					strings.Contains(t, "послезавтра") || strings.Contains(t, "бүрсікүні") ||
					strings.Contains(t, "сегодня") || strings.Contains(t, "бүгін")) &&
					proposed.Before(now) {
					proposed = proposed.AddDate(0, 0, 1)
				}
				return proposed, true
			}
		}
		return time.Time{}, false
	}

	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if h < 0 || h > 23 || min < 0 || min > 59 {
		return time.Time{}, false
	}

	res := time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, loc)
	// если день не указан явно и время уже прошло — на завтра
	if !(strings.Contains(t, "завтра") || strings.Contains(t, "ертең") ||
		strings.Contains(t, "послезавтра") || strings.Contains(t, "бүрсікүні") ||
		strings.Contains(t, "сегодня") || strings.Contains(t, "бүгін")) &&
		res.Before(now) {
		res = res.AddDate(0, 0, 1)
	}
	return res, true
}

// ============================================================================
//                       Старт с предгенерацией реплик
// ============================================================================

func startVoiceSession(phone string) error {
	// 1) получаем пациента и текст
	infoResp, err := fetchPatientInfo(phone)
	if err != nil {
		return fmt.Errorf("fetchPatientInfo: %w", err)
	}
	if strings.TrimSpace(infoResp.Patient.Name) == "" {
		return fmt.Errorf("patient name is empty")
	}
	if strings.TrimSpace(infoResp.Information) == "" {
		return fmt.Errorf("information is empty")
	}

	// 2) генерим все реплики RU/KK заранее
	baseName := "dlg_" + phone
	pack, err := pregenPrompts(baseName, infoResp.Patient.Name, infoResp.Information)
	if err != nil {
		return fmt.Errorf("pregenPrompts: %w", err)
	}
	currentPack = pack
	chosenLang = "ru"

	// 3) начальный prompt — вопрос про язык (русская версия)
	if b, err := os.ReadFile(currentPack.AskLangRU_ALaw); err == nil {
		_ = os.WriteFile(playbackFile, b, 0644)
	} else {
		return fmt.Errorf("load ask_lang alaw: %w", err)
	}

	log.Printf("Initial prompts pre-generated for %s", phone)
	return nil
}

// ============================================================================
//                               Вспомогательное
// ============================================================================

func playBeepRTP(rtpConn *net.UDPConn, remoteAddr *net.UDPAddr) error {
	sampleRate := 8000
	durationMs := 400
	freq := 1000.0
	numSamples := sampleRate * durationMs / 1000
	frameSize := 160
	seq := uint16(r.Uint32())
	ts := uint32(time.Now().Unix()%65536) * 160
	ssrc := r.Uint32()
	buf := make([]byte, frameSize)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < numSamples; i++ {
		val := int16(32000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		buf[i%frameSize] = linearToALaw(val)
		if (i+1)%frameSize == 0 {
			pkt := buildRTPPacket(8, seq, ts, ssrc, buf)
			if _, err := rtpConn.WriteToUDP(pkt, remoteAddr); err != nil {
				return err
			}
			seq++
			ts += uint32(frameSize)
			<-ticker.C
			for j := range buf {
				buf[j] = 0
			}
		}
	}
	if rem := numSamples % frameSize; rem != 0 {
		pkt := buildRTPPacket(8, seq, ts, ssrc, buf[:rem])
		if _, err := rtpConn.WriteToUDP(pkt, remoteAddr); err != nil {
			return err
		}
	}
	return nil
}

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

var r = rand.New(rand.NewSource(time.Now().UnixNano()))
var selectedPayloadType = 8

func buildBranch() string { return fmt.Sprintf("z9hG4bK%08x", r.Uint32()) }
func buildTag() string    { return fmt.Sprintf("t%08x", r.Uint32()) }
func buildCallID() string { return fmt.Sprintf("%d@%s", time.Now().UnixNano(), sipServer) }

func parseStatusCode(msg string) int {
	lines := strings.Split(msg, "\r\n")
	if len(lines) == 0 {
		return 0
	}
	var code int
	fmt.Sscanf(lines[0], "SIP/2.0 %d", &code)
	return code
}

var (
	reRealm = regexp.MustCompile(`realm="([^"]+)"`)
	reNonce = regexp.MustCompile(`nonce="([^"]+)"`)
	reToTag = regexp.MustCompile(`(?i)^To:.*;tag=([^;\r\n]+)`)
)

func extract(str string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(str)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

func send(conn *net.UDPConn, data string) error {
	_, err := conn.Write([]byte(data))
	return err
}

func readPacket(conn *net.UDPConn) (string, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

type digestAuth struct{ realm, nonce string }

func (d *digestAuth) computeResponse(method, uri string) string {
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", username, d.realm, password))
	ha2 := md5Hex(fmt.Sprintf("%s:%s", method, uri))
	return md5Hex(fmt.Sprintf("%s:%s:%s", ha1, d.nonce, ha2))
}

func buildRegister(localIP string, localPort int, callID, fromTag string, cseq int, auth *digestAuth) string {
	branch := buildBranch()
	uri := fmt.Sprintf("sip:%s", sipServer)
	var authLine string
	if auth != nil {
		resp := auth.computeResponse("REGISTER", uri)
		authLine = fmt.Sprintf("Authorization: Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\", algorithm=MD5\r\n", username, auth.realm, auth.nonce, uri, resp)
	}
	return fmt.Sprintf("REGISTER %s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=%s;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: \"%s\" <sip:%s@%s>;tag=%s\r\n"+
		"To: \"%s\" <sip:%s@%s>\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d REGISTER\r\n"+
		"Contact: <sip:%s@%s:%d>\r\n"+
		"Expires: %d\r\n"+
		"User-Agent: %s\r\n"+
		"Allow: INVITE, ACK, BYE, REGISTER, CANCEL, OPTIONS\r\n"+
		"%s"+
		"Content-Length: 0\r\n\r\n",
		uri,
		localIP, localPort, branch,
		username, username, sipServer, fromTag,
		username, username, sipServer,
		callID,
		cseq,
		username, localIP, localPort,
		expiration,
		userAgent,
		authLine,
	)
}

func buildInvite(localIP string, localPort int, mediaPort int, callID, fromTag string, toNumber string, cseq int, auth *digestAuth) string {
	branch := buildBranch()
	uri := fmt.Sprintf("sip:%s@%s", toNumber, sipServer)
	var authLine string
	if auth != nil {
		resp := auth.computeResponse("INVITE", uri)
		authLine = fmt.Sprintf("Authorization: Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\", algorithm=MD5\r\n", username, auth.realm, auth.nonce, uri, resp)
	}
	sdp := fmt.Sprintf("v=0\r\n"+
		"o=%s %d %d IN IP4 %s\r\n"+
		"s=-\r\n"+
		"c=IN IP4 %s\r\n"+
		"t=0 0\r\n"+
		"m=audio %d RTP/AVP 8 0\r\n"+
		"a=rtpmap:8 PCMA/8000\r\n"+
		"a=rtpmap:0 PCMU/8000\r\n",
		username, time.Now().Unix(), time.Now().Unix(), localIP,
		localIP,
		mediaPort,
	)
	bodyLen := len(sdp)
	return fmt.Sprintf("INVITE %s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=%s;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: \"%s\" <sip:%s@%s>;tag=%s\r\n"+
		"To: <sip:%s@%s>\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d INVITE\r\n"+
		"Contact: <sip:%s@%s:%d>\r\n"+
		"User-Agent: %s\r\n"+
		"Allow: INVITE, ACK, BYE, REGISTER, CANCEL, OPTIONS\r\n"+
		"%s"+
		"Content-Type: application/sdp\r\n"+
		"Content-Length: %d\r\n\r\n%s",
		uri,
		localIP, localPort, branch,
		username, username, sipServer, fromTag,
		toNumber, sipServer,
		callID,
		cseq,
		username, localIP, localPort,
		userAgent,
		authLine,
		bodyLen, sdp,
	)
}

func buildAck(localIP string, localPort int, callID, fromTag, toTag, toNumber string, inviteCSeq int) string {
	branch := buildBranch()
	uri := fmt.Sprintf("sip:%s@%s", toNumber, sipServer)
	return fmt.Sprintf("ACK %s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=%s;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: \"%s\" <sip:%s@%s>;tag=%s\r\n"+
		"To: <sip:%s@%s>;tag=%s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d ACK\r\n"+
		"Content-Length: 0\r\n\r\n",
		uri,
		localIP, localPort, branch,
		username, username, sipServer, fromTag,
		toNumber, sipServer, toTag,
		callID,
		inviteCSeq,
	)
}

func buildBye(localIP string, localPort int, callID, fromTag, toTag, toNumber string, cseq int) string {
	branch := buildBranch()
	uri := fmt.Sprintf("sip:%s@%s", toNumber, sipServer)
	return fmt.Sprintf("BYE %s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=%s;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: \"%s\" <sip:%s@%s>;tag=%s\r\n"+
		"To: <sip:%s@%s>;tag=%s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d BYE\r\n"+
		"Content-Length: 0\r\n\r\n",
		uri,
		localIP, localPort, branch,
		username, username, sipServer, fromTag,
		toNumber, sipServer, toTag,
		callID,
		cseq,
	)
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\r\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func parseSDP(body string, fallbackIP string) (ip string, port int) {
	ip = fallbackIP
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "c=") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				ip = parts[2]
			}
		} else if strings.HasPrefix(line, "m=audio") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				fmt.Sscanf(parts[1], "%d", &port)
			}
		}
	}
	return
}

// ---------- RTP ----------

var bargeIn int32 // 0/1

func buildRTPPacket(pt uint8, seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	hdr := make([]byte, 12)
	hdr[0] = 0x80
	hdr[1] = pt & 0x7F
	hdr[2] = byte(seq >> 8)
	hdr[3] = byte(seq)
	hdr[4] = byte(ts >> 24)
	hdr[5] = byte(ts >> 16)
	hdr[6] = byte(ts >> 8)
	hdr[7] = byte(ts)
	hdr[8] = byte(ssrc >> 24)
	hdr[9] = byte(ssrc >> 16)
	hdr[10] = byte(ssrc >> 8)
	hdr[11] = byte(ssrc)
	return append(hdr, payload...)
}

// плеер без barge-in — всегда доигрывает файл до конца
func rtpPlayOnConn(rtpConn *net.UDPConn, remoteAddr *net.UDPAddr, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open playback file: %w", err)
	}
	defer f.Close()

	seq := uint16(r.Uint32())
	ts := uint32(time.Now().Unix()%65536) * 160
	ssrc := r.Uint32()

	const frameSize = 160
	buf := make([]byte, frameSize)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		n, err := io.ReadFull(f, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}

		payload := buf[:n]
		pkt := buildRTPPacket(8, seq, ts, ssrc, payload)
		if _, err := rtpConn.WriteToUDP(pkt, remoteAddr); err != nil {
			return err
		}
		seq++
		ts += uint32(frameSize)
		<-ticker.C
	}
	return nil
}

// rtpRecordFixedDuration — пишет входящий RTP (PCMA/PCMU) ровно dur,
// сохраняет «сырые» A-law байты в outFile. Без VAD, без бардж-ина.
func rtpRecordFixedDuration(rtpConn *net.UDPConn, outFile string, dur time.Duration) error {
	accepted := map[byte]bool{8: true, 0: true} // PCMA/PCMU
	buf := make([]byte, 1500)

	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// Жёсткий дедлайн конца записи
	endAt := time.Now().Add(dur)

	// Небольшой быстрый дренаж входного буфера (если что-то зависло с прошлого цикла)
	drainUntil := time.Now().Add(80 * time.Millisecond)
	for time.Now().Before(drainUntil) {
		_ = rtpConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if n, _, err := rtpConn.ReadFromUDP(buf); err == nil && n >= 12 {
			// просто выкидываем
		} else {
			break
		}
	}

	for time.Now().Before(endAt) {
		// короткий дедлайн на каждый приём, чтобы не виснуть
		if err := rtpConn.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
			return err
		}
		n, _, err := rtpConn.ReadFromUDP(buf)
		if err != nil {
			// таймаут — просто ждём дальше до конца окна записи
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// сетевые ошибки — выйдем
			return err
		}
		if n < 12 {
			continue
		}
		pt := buf[1] & 0x7F
		if !accepted[pt] {
			continue
		}
		payload := buf[12:n]
		if len(payload) == 0 {
			continue
		}
		if _, err := f.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// VAD-запись: до 200мс тишины ПОСЛЕ начала речи
// VAD-запись: пишем только речь, стоп после 200мс тишины
func rtpRecordUntilSilence(rtpConn *net.UDPConn, outFile string) error {
	accepted := map[byte]bool{8: true, 0: true} // PCMA/PCMU
	frameDur := 20 * time.Millisecond
	const tailSilenceNeed = 10       // 10*20ms = 200ms
	const maxTotal = 5 * time.Second // верхняя «крышка», чтобы не виснуть
	const minSpeechFrames = 10       // минимум 200ms речи
	const rmsThresh = 600            // порог VAD

	buf := make([]byte, 1500)

	// КОРОТКИЙ ДРЕЙН — выбросить «хвосты» предыдущей фазы
	drainUntil := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(drainUntil) {
		_ = rtpConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if n, _, err := rtpConn.ReadFromUDP(buf); err == nil && n >= 12 {
			// discard
		} else {
			break
		}
	}

	// МАЛЕНЬКАЯ ПАУЗА перед началом записи, чтобы не поймать свой же TTS
	time.Sleep(120 * time.Millisecond)

	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer f.Close()

	isSilent := func(payload []byte, pt byte) bool {
		if len(payload) == 0 {
			return true
		}
		var sum2 int64
		n := 0
		if pt == 0 { // PCMU
			for _, b := range payload {
				s := ulawToLinear(b)
				sum2 += int64(s) * int64(s)
				n++
			}
		} else { // PCMA
			for _, b := range payload {
				s := alawToLinear(b)
				sum2 += int64(s) * int64(s)
				n++
			}
		}
		if n == 0 {
			return true
		}
		rms := math.Sqrt(float64(sum2) / float64(n))
		return rms < rmsThresh
	}

	start := time.Now()
	heardSpeech := false
	speechFrames := 0
	silenceFrames := 0

	deadline := time.Now().Add(300 * time.Millisecond)

	for time.Since(start) < maxTotal {
		rtpConn.SetReadDeadline(deadline)
		n, _, err := rtpConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				deadline = time.Now().Add(300 * time.Millisecond)
				continue
			}
			return err
		}
		if n < 12 {
			continue
		}
		pt := buf[1] & 0x7F
		if !accepted[pt] {
			continue
		}
		payload := buf[12:n]
		if len(payload) == 0 {
			continue
		}

		if isSilent(payload, pt) {
			if heardSpeech {
				silenceFrames++
				if silenceFrames >= tailSilenceNeed && speechFrames >= minSpeechFrames {
					break
				}
			}
		} else {
			if !heardSpeech {
				heardSpeech = true
				speechFrames = 0
				silenceFrames = 0
				// Сброс бардж-ина для будущего использования
				atomic.StoreInt32(&bargeIn, 1)
			}
			if heardSpeech {
				if _, err := f.Write(payload); err != nil {
					return err
				}
				speechFrames++
				silenceFrames = 0
			}
		}
		deadline = time.Now().Add(frameDur + 100*time.Millisecond)
	}
	return nil
}

// ---------- Кодеки/аудио утилиты ----------

func ulawToWav(inPath, outPath string, sampleRate int) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize := uint32(len(data))
	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	riffSize := uint32(36) + dataSize
	if err := binary.Write(f, binary.LittleEndian, riffSize); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVE")); err != nil {
		return err
	}
	if _, err := f.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(7)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	byteRate := uint32(sampleRate * 1 * 1)
	if err := binary.Write(f, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(8)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return nil
}

func alawToWav(inPath, outPath string, sampleRate int) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize := uint32(len(data))
	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	riffSize := uint32(36) + dataSize
	if err := binary.Write(f, binary.LittleEndian, riffSize); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVE")); err != nil {
		return err
	}
	if _, err := f.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(6)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	byteRate := uint32(sampleRate * 1 * 1)
	if err := binary.Write(f, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(8)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return nil
}

func ulawRawToPCM16Wav(inPath, outPath string, sampleRate int) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	pcm := make([]int16, len(data))
	for i, b := range data {
		pcm[i] = ulawToLinear(b)
	}
	return writePCM16ToWav(outPath, pcm, sampleRate)
}

func alawRawToPCM16Wav(inPath, outPath string, sampleRate int) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	pcm := make([]int16, len(data))
	for i, b := range data {
		pcm[i] = alawToLinear(b)
	}
	return writePCM16ToWav(outPath, pcm, sampleRate)
}

func writePCM16ToWav(outPath string, pcm []int16, sampleRate int) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize := uint32(len(pcm) * 2)
	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(36)+dataSize); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVE")); err != nil {
		return err
	}
	if _, err := f.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate*2)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(2)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(16)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	for _, s := range pcm {
		if err := binary.Write(f, binary.LittleEndian, s); err != nil {
			return err
		}
	}
	return nil
}

func ulawToLinear(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exp := (u >> 4) & 0x07
	mant := u & 0x0F
	value := (int(mant) << 3) + 0x84
	value <<= uint(exp)
	value -= 0x84
	if sign != 0 {
		return int16(-value)
	}
	return int16(value)
}

func alawToLinear(a byte) int16 {
	a ^= 0x55
	sign := a & 0x80
	exp := (a >> 4) & 0x07
	mant := a & 0x0F
	var value int
	if exp != 0 {
		value = (int(mant)<<4 + 0x100) << (exp - 1)
	} else {
		value = (int(mant) << 4) + 8
	}
	if sign != 0 {
		return int16(-value)
	}
	return int16(value)
}

func linearToALaw(sample int16) byte {
	const cClip = 32635
	sign := byte(0)
	if sample < 0 {
		sample = -sample - 1
		sign = 0x80
	}
	if sample > cClip {
		sample = cClip
	}
	var comp byte
	if sample >= 256 {
		exp := byte(7)
		for mask := int16(0x4000); (sample&mask) == 0 && exp > 0; mask >>= 1 {
			exp--
		}
		mant := byte((sample >> (int(exp) + 3)) & 0x0F)
		comp = (exp << 4) | mant
	} else {
		comp = byte(sample >> 4)
	}
	comp ^= 0x55
	return comp ^ sign
}

func getWavSecs(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, 44)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, err
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, fmt.Errorf("not WAV")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	buf := append(hdr, data...)
	pos := 12
	var dataSize int
	for pos+8 <= len(buf) {
		id := string(buf[pos : pos+4])
		sz := int(binary.LittleEndian.Uint32(buf[pos+4 : pos+8]))
		pos += 8
		if id == "data" {
			dataSize = sz
			break
		}
		pos += sz
		if pos%2 == 1 {
			pos++
		}
	}
	if dataSize == 0 {
		return 0, fmt.Errorf("no data chunk")
	}
	channels := int(binary.LittleEndian.Uint16(hdr[22:24]))
	rate := int(binary.LittleEndian.Uint32(hdr[24:28]))
	bps := int(binary.LittleEndian.Uint16(hdr[34:36]))
	if channels <= 0 || rate <= 0 || bps <= 0 {
		return 0, fmt.Errorf("bad header")
	}
	bytesPerSample := channels * (bps / 8)
	if bytesPerSample == 0 {
		return 0, fmt.Errorf("zero bytesPerSample")
	}
	samples := dataSize / bytesPerSample
	return float64(samples) / float64(rate), nil
}

// ============================================================================
//                          Диалог / API / TTS / STT
// ============================================================================

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func getAPIBase() string {
	base := os.Getenv("BASE_API")
	if base == "" {
		base = "http://localhost:8080/v1/voice"
	}
	return strings.TrimRight(base, "/")
}

func looksLikeWav(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

func looksLikeMP3(b []byte) bool {
	return (len(b) >= 3 && string(b[:3]) == "ID3") ||
		(len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0)
}

// TTS: в WAV → нормализуем → A-law RAW
func GenerateOpenAITTS(text, outWavPath string) error {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY not set")
	}

	reqBody := map[string]any{
		"model":        "gpt-4o-mini-tts",
		"voice":        "nova",
		"format":       "wav",
		"instructions": "Голос: мягкий, нежный, успокаивающий; олицетворяет спокойствие.\nТон: спокойный, уверяющий, умиротворённый; передаёт искреннее тепло и ощущение гармонии.\nТемп речи: медленный, размеренный, без спешки; делай мягкие паузы после инструкций, чтобы слушатель успевал расслабиться и следовать за голосом.\nЭмоции: глубоко успокаивающие и заботливые; транслируй искреннюю доброту и заботу.\nПроизношение: плавное, мягкое, с лёгким удлинением гласных, чтобы создавать ощущение лёгкости и уюта.\nПаузы: обдуманные и естественные, особенно между инструкциями по дыханию и визуализации, чтобы усилить эффект расслабления и осознанности.",
		"input":        text,
	}
	jb, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", bytes.NewReader(jb))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("OpenAI TTS %d: %s", resp.StatusCode, truncate(string(b), 400))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))

	switch {
	case looksLikeWav(b) || strings.Contains(ct, "audio/wav") || strings.Contains(ct, "audio/x-wav"):
		if err := os.WriteFile(outWavPath, b, 0644); err != nil {
			return err
		}
	case looksLikeMP3(b) || strings.Contains(ct, "audio/mpeg"):
		tmpMP3 := outWavPath + ".mp3"
		if err := os.WriteFile(tmpMP3, b, 0644); err != nil {
			return err
		}
		if err := mp3ToPCM16Wav(tmpMP3, outWavPath); err != nil {
			return fmt.Errorf("mp3ToPCM16Wav: %w", err)
		}
	case strings.Contains(ct, "application/json") || (len(b) > 0 && b[0] == '{'):
		return fmt.Errorf("OpenAI TTS returned JSON instead of audio: %s", truncate(string(b), 400))
	default:
		return fmt.Errorf("OpenAI TTS: unknown audio format (Content-Type=%q)", ct)
	}

	norm := strings.TrimSuffix(outWavPath, ".wav") + "_8k_mono.wav"
	if err := ensureWavPCM16Mono8kNoFFMPEG(outWavPath, norm); err != nil {
		return fmt.Errorf("normalize WAV: %w", err)
	}
	if err := wavToALawRaw(norm, playbackFile); err != nil {
		return fmt.Errorf("wavToALawRaw: %w", err)
	}
	return nil
}

// Быстрая STT через OpenAI API
func transcribeOpenAI(wavPath string, preferredLang string) (string, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not set")
	}

	f, err := os.Open(wavPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	fw, _ := w.CreateFormFile("file", filepath.Base(wavPath))
	io.Copy(fw, f)

	_ = w.WriteField("model", "gpt-4o-mini-transcribe")
	if preferredLang == "ru" || preferredLang == "kk" {
		_ = w.WriteField("language", preferredLang)
	}
	_ = w.WriteField("response_format", "text") // ← ВАЖНО

	w.Close()

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI STT %d: %s", resp.StatusCode, truncate(string(b), 400))
	}
	// вернётся чистое "Нет." без JSON и кавычек
	return strings.TrimSpace(string(b)), nil
}

// WAV нормализация / ресемпл

type wavInfo struct {
	AudioFormat     uint16
	NumCh           uint16
	SampleRate      uint32
	BitsPerSample   uint16
	ExtensiblePCM   bool
	ExtensibleFloat bool
	Data            []byte
}

func readWavPCM(w []byte) (wavInfo, error) {
	var wi wavInfo
	if len(w) < 44 || string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" {
		return wi, fmt.Errorf("not a WAV")
	}
	pos := 12
	var gotFmt, gotData bool

	for pos+8 <= len(w) {
		chunkID := string(w[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(w[pos+4 : pos+8]))
		pos += 8
		if pos+chunkSize > len(w) {
			return wi, fmt.Errorf("truncated chunk %s", chunkID)
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return wi, fmt.Errorf("fmt too small")
			}
			wi.AudioFormat = binary.LittleEndian.Uint16(w[pos : pos+2])
			wi.NumCh = binary.LittleEndian.Uint16(w[pos+2 : pos+4])
			wi.SampleRate = binary.LittleEndian.Uint32(w[pos+4 : pos+8])
			wi.BitsPerSample = binary.LittleEndian.Uint16(w[pos+14 : pos+16])
			if chunkSize > 16 {
				if wi.AudioFormat == 0xFFFE && chunkSize >= 40 {
					cbSize := binary.LittleEndian.Uint16(w[pos+16 : pos+18])
					if cbSize >= 22 {
						sub := w[pos+24 : pos+40]
						if binary.LittleEndian.Uint32(sub[0:4]) == 1 {
							wi.ExtensiblePCM = true
						} else if binary.LittleEndian.Uint32(sub[0:4]) == 3 {
							wi.ExtensibleFloat = true
						}
					}
				}
			}
			gotFmt = true
		case "data":
			if !gotData {
				wi.Data = append([]byte(nil), w[pos:pos+chunkSize]...)
				gotData = true
			}
		default:
		}
		pos += chunkSize
		if pos%2 == 1 {
			pos++
		}
	}

	if !gotFmt {
		return wi, fmt.Errorf("no fmt chunk")
	}
	if !gotData {
		return wi, fmt.Errorf("no data chunk")
	}
	if wi.NumCh == 0 || wi.SampleRate == 0 || wi.BitsPerSample == 0 {
		return wi, fmt.Errorf("bad header")
	}
	return wi, nil
}

func writeWavPCM16Mono(outPath string, pcm []int16, sampleRate int) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize := uint32(len(pcm) * 2)
	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(36)+dataSize); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVE")); err != nil {
		return err
	}
	if _, err := f.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate*2)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(2)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(16)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	for _, s := range pcm {
		if err := binary.Write(f, binary.LittleEndian, s); err != nil {
			return err
		}
	}
	return nil
}

func decodeToPCM16All(wi wavInfo) ([]int16, error) {
	isPCM := wi.AudioFormat == 1 || (wi.AudioFormat == 0xFFFE && wi.ExtensiblePCM)
	isFloat := wi.AudioFormat == 3 || (wi.AudioFormat == 0xFFFE && wi.ExtensibleFloat)

	switch {
	case isPCM && wi.BitsPerSample == 16:
		if len(wi.Data)%2 != 0 {
			return nil, fmt.Errorf("odd 16-bit data")
		}
		s := make([]int16, len(wi.Data)/2)
		for i := 0; i < len(s); i++ {
			s[i] = int16(binary.LittleEndian.Uint16(wi.Data[i*2 : i*2+2]))
		}
		return s, nil

	case isPCM && wi.BitsPerSample == 24:
		if len(wi.Data)%3 != 0 {
			return nil, fmt.Errorf("bad 24-bit data")
		}
		n := len(wi.Data) / 3
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			b0 := int32(wi.Data[i*3+0])
			b1 := int32(wi.Data[i*3+1])
			b2 := int32(wi.Data[i*3+2])
			val := (b2<<16 | b1<<8 | b0)
			if val&0x800000 != 0 {
				val |= ^0xFFFFFF
			}
			out[i] = int16(val >> 8)
		}
		return out, nil

	case isPCM && wi.BitsPerSample == 32:
		if len(wi.Data)%4 != 0 {
			return nil, fmt.Errorf("bad 32-bit data")
		}
		n := len(wi.Data) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			u := binary.LittleEndian.Uint32(wi.Data[i*4 : i*4+4])
			val := int32(u)
			out[i] = int16(val >> 16)
		}
		return out, nil

	case isFloat && wi.BitsPerSample == 32:
		if len(wi.Data)%4 != 0 {
			return nil, fmt.Errorf("bad float32 data")
		}
		n := len(wi.Data) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			bits := binary.LittleEndian.Uint32(wi.Data[i*4 : i*4+4])
			f := math.Float32frombits(bits)
			if f > 1 {
				f = 1
			}
			if f < -1 {
				f = -1
			}
			out[i] = int16(f * 32767.0)
		}
		return out, nil
	}

	return nil, fmt.Errorf("unsupported WAV combination: fmt=%d, bits=%d (extPCM=%v, extFloat=%v)",
		wi.AudioFormat, wi.BitsPerSample, wi.ExtensiblePCM, wi.ExtensibleFloat)
}

func toMono(samples []int16, channels int) []int16 {
	if channels <= 1 {
		return samples
	}
	out := make([]int16, len(samples)/channels)
	for i := 0; i < len(out); i++ {
		sum := 0
		for ch := 0; ch < channels; ch++ {
			sum += int(samples[i*channels+ch])
		}
		out[i] = int16(sum / channels)
	}
	return out
}

func resampleLinear(in []int16, inRate, outRate int) []int16 {
	if inRate == outRate || len(in) == 0 {
		return append([]int16(nil), in...)
	}
	ratio := float64(outRate) / float64(inRate)
	outLen := int(math.Round(float64(len(in)) * ratio))
	if outLen <= 1 {
		return []int16{}
	}
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		srcPos := float64(i) / ratio
		i0 := int(math.Floor(srcPos))
		i1 := i0 + 1
		if i1 >= len(in) {
			i1 = len(in) - 1
		}
		f := srcPos - float64(i0)
		v := float64(in[i0])*(1.0-f) + float64(in[i1])*f
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		out[i] = int16(v)
	}
	return out
}

// лёгкая обработка вокала
func processVoicePCM(pcm []int16) []int16 {
	if len(pcm) == 0 {
		return pcm
	}
	var acc int64
	for _, s := range pcm {
		acc += int64(s)
	}
	offset := int16(acc / int64(len(pcm)))
	for i := range pcm {
		pcm[i] -= offset
	}
	maxAbs := 1
	for _, s := range pcm {
		a := int(s)
		if a < 0 {
			a = -a
		}
		if a > maxAbs {
			maxAbs = a
		}
	}
	target := int(math.Round(0.89125 * 32767)) // ≈ -1 dBFS
	gain := float64(target) / float64(maxAbs)
	if gain > 1.3 {
		gain = 1.3
	}
	for i := range pcm {
		v := float64(pcm[i]) * gain
		v = math.Tanh(v/32767.0) * 32767.0
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		pcm[i] = int16(v)
	}
	return pcm
}

// гарантируем PCM16 mono 8k WAV
func ensureWavPCM16Mono8kNoFFMPEG(inPath, outPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	wi, err := readWavPCM(data)
	if err != nil {
		return err
	}
	raw, err := decodeToPCM16All(wi)
	if err != nil {
		return err
	}
	mono := toMono(raw, int(wi.NumCh))
	mono = processVoicePCM(mono)
	out := resampleLinear(mono, int(wi.SampleRate), 8000)
	return writeWavPCM16Mono(outPath, out, 8000)
}

// MP3 → WAV PCM16 mono 8k
func mp3ToPCM16Wav(inPath, outPath string) error {
	f, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer f.Close()

	dec, err := mp3.NewDecoder(f)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(dec)
	if err != nil {
		return err
	}
	if len(raw)%4 != 0 {
		return fmt.Errorf("unexpected MP3 decoded length")
	}

	// стерео 16-bit LE → []int16
	samples := make([]int16, len(raw)/2)
	for i := 0; i < len(samples); i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2]))
	}

	// downmix в моно
	mono := make([]int16, len(samples)/2)
	for i := 0; i < len(mono); i++ {
		L := int(samples[2*i])
		R := int(samples[2*i+1])
		mono[i] = int16((L + R) / 2)
	}

	mono = processVoicePCM(mono)
	out := resampleLinear(mono, dec.SampleRate(), 8000)

	return writeWavPCM16Mono(outPath, out, 8000)
}

// ---------- WAV → A-law RAW ----------

func wavToALawRaw(inPath, outPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return fmt.Errorf("not a WAV")
	}
	pos := 12
	var audioFormat, numCh int
	var sampleRate int
	var pcmData []byte
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if pos+chunkSize > len(data) {
			break
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return fmt.Errorf("fmt chunk too small")
			}
			audioFormat = int(binary.LittleEndian.Uint16(data[pos : pos+2]))
			numCh = int(binary.LittleEndian.Uint16(data[pos+2 : pos+4]))
			sampleRate = int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		case "data":
			pcmData = data[pos : pos+chunkSize]
		}
		pos += chunkSize
		if pos%2 == 1 {
			pos++
		}
	}
	if audioFormat != 1 {
		return fmt.Errorf("unsupported audio format %d (need PCM)", audioFormat)
	}
	if numCh != 1 {
		return fmt.Errorf("need mono, got %d", numCh)
	}
	if sampleRate != 8000 {
		return fmt.Errorf("need 8000Hz, got %d", sampleRate)
	}
	if len(pcmData)%2 != 0 {
		return fmt.Errorf("odd pcm data length")
	}
	out := make([]byte, len(pcmData)/2)
	for i := 0; i < len(pcmData); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcmData[i : i+2]))
		out[i/2] = linearToALaw(s)
	}
	return os.WriteFile(outPath, out, 0644)
}
