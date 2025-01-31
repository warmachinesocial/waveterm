// Copyright 2023, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package cmdrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/wavetermdev/waveterm/waveshell/pkg/base"
	"github.com/wavetermdev/waveterm/waveshell/pkg/packet"
	"github.com/wavetermdev/waveterm/waveshell/pkg/shexec"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/comp"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/dbutil"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/pcloud"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/remote"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/remote/openai"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/scbase"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/scpacket"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/sstore"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/utilfn"
)

const (
	HistoryTypeScreen  = "screen"
	HistoryTypeSession = "session"
	HistoryTypeGlobal  = "global"
)

func init() {
	comp.RegisterSimpleCompFn(comp.CGTypeMeta, simpleCompMeta)
	comp.RegisterSimpleCompFn(comp.CGTypeCommandMeta, simpleCompCommandMeta)
}

const DefaultUserId = "user"
const MaxNameLen = 50
const MaxShareNameLen = 150
const MaxRendererLen = 50
const MaxRemoteAliasLen = 50
const PasswordUnchangedSentinel = "--unchanged--"
const DefaultPTERM = "MxM"
const MaxCommandLen = 4096
const MaxSignalLen = 12
const MaxSignalNum = 64
const MaxEvalDepth = 5
const MaxOpenAIAPITokenLen = 100
const MaxOpenAIModelLen = 100

const TermFontSizeMin = 8
const TermFontSizeMax = 24

const TsFormatStr = "2006-01-02 15:04:05"

const (
	KwArgRenderer = "renderer"
	KwArgView     = "view"
	KwArgState    = "state"
	KwArgTemplate = "template"
	KwArgLang     = "lang"
)

var ColorNames = []string{"yellow", "blue", "pink", "mint", "cyan", "violet", "orange", "green", "red", "white"}
var TabIcons = []string{"square", "sparkle", "fire", "ghost", "cloud", "compass", "crown", "droplet", "graduation-cap", "heart", "file"}
var RemoteColorNames = []string{"red", "green", "yellow", "blue", "magenta", "cyan", "white", "orange"}
var RemoteSetArgs = []string{"alias", "connectmode", "key", "password", "autoinstall", "color"}

var ScreenCmds = []string{"run", "comment", "cd", "cr", "clear", "sw", "reset", "signal", "chat"}
var NoHistCmds = []string{"_compgen", "line", "history", "_killserver"}
var GlobalCmds = []string{"session", "screen", "remote", "set", "client", "telemetry", "bookmark", "bookmarks"}

var SetVarNameMap map[string]string = map[string]string{
	"tabcolor": "screen.tabcolor",
	"tabicon":  "screen.tabicon",
	"pterm":    "screen.pterm",
	"anchor":   "screen.anchor",
	"focus":    "screen.focus",
	"line":     "screen.line",
}

var SetVarScopes = []SetVarScope{
	SetVarScope{ScopeName: "global", VarNames: []string{}},
	SetVarScope{ScopeName: "client", VarNames: []string{"telemetry"}},
	SetVarScope{ScopeName: "session", VarNames: []string{"name", "pos"}},
	SetVarScope{ScopeName: "screen", VarNames: []string{"name", "tabcolor", "tabicon", "pos", "pterm", "anchor", "focus", "line"}},
	SetVarScope{ScopeName: "line", VarNames: []string{}},
	// connection = remote, remote = remoteinstance
	SetVarScope{ScopeName: "connection", VarNames: []string{"alias", "connectmode", "key", "password", "autoinstall", "color"}},
	SetVarScope{ScopeName: "remote", VarNames: []string{}},
}

var hostNameRe = regexp.MustCompile("^[a-z][a-z0-9.-]*$")
var userHostRe = regexp.MustCompile("^(sudo@)?([a-z][a-z0-9._-]*)@([a-z0-9][a-z0-9.-]*)(?::([0-9]+))?$")
var remoteAliasRe = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9_-]*$")
var genericNameRe = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9_ .()<>,/\"'\\[\\]{}=+$@!*-]*$")
var rendererRe = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9_.:-]*$")
var positionRe = regexp.MustCompile("^((S?\\+|E?-)?[0-9]+|(\\+|-|S|E))$")
var wsRe = regexp.MustCompile("\\s+")
var sigNameRe = regexp.MustCompile("^((SIG[A-Z0-9]+)|(\\d+))$")

type contextType string

var historyContextKey = contextType("history")
var depthContextKey = contextType("depth")

type SetVarScope struct {
	ScopeName string
	VarNames  []string
}

type historyContextType struct {
	LineId    string
	LineNum   int64
	RemotePtr *sstore.RemotePtrType
}

type MetaCmdFnType = func(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error)
type MetaCmdEntryType struct {
	IsAlias bool
	Fn      MetaCmdFnType
}

var MetaCmdFnMap = make(map[string]MetaCmdEntryType)

func init() {
	registerCmdFn("run", RunCommand)
	registerCmdFn("eval", EvalCommand)
	registerCmdFn("comment", CommentCommand)
	registerCmdFn("cr", CrCommand)
	registerCmdFn("connect", CrCommand)
	registerCmdFn("_compgen", CompGenCommand)
	registerCmdFn("clear", ClearCommand)
	registerCmdFn("reset", RemoteResetCommand)
	registerCmdFn("signal", SignalCommand)
	registerCmdFn("sync", SyncCommand)

	registerCmdFn("session", SessionCommand)
	registerCmdFn("session:open", SessionOpenCommand)
	registerCmdAlias("session:new", SessionOpenCommand)
	registerCmdFn("session:set", SessionSetCommand)
	registerCmdAlias("session:delete", SessionDeleteCommand)
	registerCmdFn("session:purge", SessionDeleteCommand)
	registerCmdFn("session:archive", SessionArchiveCommand)
	registerCmdFn("session:showall", SessionShowAllCommand)
	registerCmdFn("session:show", SessionShowCommand)
	registerCmdFn("session:openshared", SessionOpenSharedCommand)

	registerCmdFn("screen", ScreenCommand)
	registerCmdFn("screen:archive", ScreenArchiveCommand)
	registerCmdFn("screen:purge", ScreenPurgeCommand)
	registerCmdFn("screen:open", ScreenOpenCommand)
	registerCmdAlias("screen:new", ScreenOpenCommand)
	registerCmdFn("screen:set", ScreenSetCommand)
	registerCmdFn("screen:showall", ScreenShowAllCommand)
	registerCmdFn("screen:reset", ScreenResetCommand)
	registerCmdFn("screen:webshare", ScreenWebShareCommand)

	registerCmdAlias("remote", RemoteCommand)
	registerCmdFn("remote:show", RemoteShowCommand)
	registerCmdFn("remote:showall", RemoteShowAllCommand)
	registerCmdFn("remote:new", RemoteNewCommand)
	registerCmdFn("remote:archive", RemoteArchiveCommand)
	registerCmdFn("remote:set", RemoteSetCommand)
	registerCmdFn("remote:disconnect", RemoteDisconnectCommand)
	registerCmdFn("remote:connect", RemoteConnectCommand)
	registerCmdFn("remote:install", RemoteInstallCommand)
	registerCmdFn("remote:installcancel", RemoteInstallCancelCommand)
	registerCmdFn("remote:reset", RemoteResetCommand)

	registerCmdFn("screen:resize", ScreenResizeCommand)

	registerCmdFn("line", LineCommand)
	registerCmdFn("line:show", LineShowCommand)
	registerCmdFn("line:star", LineStarCommand)
	registerCmdFn("line:bookmark", LineBookmarkCommand)
	registerCmdFn("line:pin", LinePinCommand)
	registerCmdFn("line:archive", LineArchiveCommand)
	registerCmdFn("line:purge", LinePurgeCommand)
	registerCmdFn("line:setheight", LineSetHeightCommand)
	registerCmdFn("line:view", LineViewCommand)
	registerCmdFn("line:set", LineSetCommand)

	registerCmdFn("client", ClientCommand)
	registerCmdFn("client:show", ClientShowCommand)
	registerCmdFn("client:set", ClientSetCommand)
	registerCmdFn("client:notifyupdatewriter", ClientNotifyUpdateWriterCommand)
	registerCmdFn("client:accepttos", ClientAcceptTosCommand)

	registerCmdFn("telemetry", TelemetryCommand)
	registerCmdFn("telemetry:on", TelemetryOnCommand)
	registerCmdFn("telemetry:off", TelemetryOffCommand)
	registerCmdFn("telemetry:send", TelemetrySendCommand)
	registerCmdFn("telemetry:show", TelemetryShowCommand)

	registerCmdFn("history", HistoryCommand)
	registerCmdFn("history:viewall", HistoryViewAllCommand)
	registerCmdFn("history:purge", HistoryPurgeCommand)

	registerCmdFn("bookmarks:show", BookmarksShowCommand)

	registerCmdFn("bookmark:set", BookmarkSetCommand)
	registerCmdFn("bookmark:delete", BookmarkDeleteCommand)

	registerCmdFn("chat", OpenAICommand)

	registerCmdFn("_killserver", KillServerCommand)

	registerCmdFn("set", SetCommand)

	registerCmdFn("view:stat", ViewStatCommand)
	registerCmdFn("view:test", ViewTestCommand)

	registerCmdFn("edit:test", EditTestCommand)

	// CodeEditCommand is overloaded to do codeedit and codeview
	registerCmdFn("codeedit", CodeEditCommand)
	registerCmdFn("codeview", CodeEditCommand)

	registerCmdFn("imageview", ImageViewCommand)
	registerCmdFn("mdview", MarkdownViewCommand)
	registerCmdFn("markdownview", MarkdownViewCommand)

	registerCmdFn("csvview", CSVViewCommand)
}

func getValidCommands() []string {
	var rtn []string
	for key, val := range MetaCmdFnMap {
		if val.IsAlias {
			continue
		}
		rtn = append(rtn, "/"+key)
	}
	return rtn
}

func registerCmdFn(cmdName string, fn MetaCmdFnType) {
	MetaCmdFnMap[cmdName] = MetaCmdEntryType{Fn: fn}
}

func registerCmdAlias(cmdName string, fn MetaCmdFnType) {
	MetaCmdFnMap[cmdName] = MetaCmdEntryType{IsAlias: true, Fn: fn}
}

func GetCmdStr(pk *scpacket.FeCommandPacketType) string {
	if pk.MetaSubCmd == "" {
		return pk.MetaCmd
	}
	return pk.MetaCmd + ":" + pk.MetaSubCmd
}

func HandleCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	metaCmd := SubMetaCmd(pk.MetaCmd)
	var cmdName string
	if pk.MetaSubCmd == "" {
		cmdName = metaCmd
	} else {
		cmdName = fmt.Sprintf("%s:%s", pk.MetaCmd, pk.MetaSubCmd)
	}
	entry := MetaCmdFnMap[cmdName]
	if entry.Fn == nil {
		if MetaCmdFnMap[metaCmd].Fn != nil {
			return nil, fmt.Errorf("invalid /%s subcommand '%s'", metaCmd, pk.MetaSubCmd)
		}
		return nil, fmt.Errorf("invalid command '/%s', no handler", cmdName)
	}
	return entry.Fn(ctx, pk)
}

func firstArg(pk *scpacket.FeCommandPacketType) string {
	if len(pk.Args) == 0 {
		return ""
	}
	return pk.Args[0]
}

func argN(pk *scpacket.FeCommandPacketType, n int) string {
	if len(pk.Args) <= n {
		return ""
	}
	return pk.Args[n]
}

func resolveBool(arg string, def bool) bool {
	if arg == "" {
		return def
	}
	if arg == "0" || arg == "false" {
		return false
	}
	return true
}

func defaultStr(arg string, def string) string {
	if arg == "" {
		return def
	}
	return arg
}

func resolveFile(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	fileName := base.ExpandHomeDir(arg)
	if !strings.HasPrefix(fileName, "/") {
		return "", fmt.Errorf("must be absolute, cannot be a relative path")
	}
	fd, err := os.Open(fileName)
	if fd != nil {
		fd.Close()
	}
	if err != nil {
		return "", fmt.Errorf("cannot open file: %v", err)
	}
	return fileName, nil
}

func resolvePosInt(arg string, def int) (int, error) {
	if arg == "" {
		return def, nil
	}
	ival, err := strconv.Atoi(arg)
	if err != nil {
		return 0, err
	}
	if ival <= 0 {
		return 0, fmt.Errorf("must be greater than 0")
	}
	return ival, nil
}

func isAllDigits(arg string) bool {
	if len(arg) == 0 {
		return false
	}
	for i := 0; i < len(arg); i++ {
		if arg[i] >= '0' && arg[i] <= '9' {
			continue
		}
		return false
	}
	return true
}

func resolveNonNegInt(arg string, def int) (int, error) {
	if arg == "" {
		return def, nil
	}
	ival, err := strconv.Atoi(arg)
	if err != nil {
		return 0, err
	}
	if ival < 0 {
		return 0, fmt.Errorf("cannot be negative")
	}
	return ival, nil
}

var histExpansionRe = regexp.MustCompile(`^!(\d+)$`)

func doCmdHistoryExpansion(ctx context.Context, ids resolvedIds, cmdStr string) (string, error) {
	if !strings.HasPrefix(cmdStr, "!") {
		return "", nil
	}
	if strings.HasPrefix(cmdStr, "! ") {
		return "", nil
	}
	if cmdStr == "!!" {
		return doHistoryExpansion(ctx, ids, -1)
	}
	if strings.HasPrefix(cmdStr, "!-") {
		return "", fmt.Errorf("wave does not support negative history offsets, use a stable positive history offset instead: '![linenum]'")
	}
	m := histExpansionRe.FindStringSubmatch(cmdStr)
	if m == nil {
		return "", fmt.Errorf("unsupported history substitution, can use '!!' or '![linenum]'")
	}
	ival, err := strconv.Atoi(m[1])
	if err != nil {
		return "", fmt.Errorf("invalid history expansion")
	}
	return doHistoryExpansion(ctx, ids, ival)
}

func doHistoryExpansion(ctx context.Context, ids resolvedIds, hnum int) (string, error) {
	if hnum == 0 {
		return "", fmt.Errorf("invalid history expansion, cannot expand line number '0'")
	}
	if hnum < -1 {
		return "", fmt.Errorf("invalid history expansion, cannot expand negative history offsets")
	}
	foundHistoryNum := hnum
	if hnum == -1 {
		var err error
		foundHistoryNum, err = sstore.GetLastHistoryLineNum(ctx, ids.ScreenId)
		if err != nil {
			return "", fmt.Errorf("cannot expand history, error finding last history item: %v", err)
		}
		if foundHistoryNum == 0 {
			return "", fmt.Errorf("cannot expand history, no last history item")
		}
	}
	hitem, err := sstore.GetHistoryItemByLineNum(ctx, ids.ScreenId, foundHistoryNum)
	if err != nil {
		return "", fmt.Errorf("cannot get history item '%d': %v", foundHistoryNum, err)
	}
	if hitem == nil {
		return "", fmt.Errorf("cannot expand history, history item '%d' not found", foundHistoryNum)
	}
	return hitem.CmdStr, nil
}

func getEvalDepth(ctx context.Context) int {
	depthVal := ctx.Value(depthContextKey)
	if depthVal == nil {
		return 0
	}
	return depthVal.(int)
}

func SyncCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, fmt.Errorf("/run error: %w", err)
	}
	runPacket := packet.MakeRunPacket()
	runPacket.ReqId = uuid.New().String()
	runPacket.CK = base.MakeCommandKey(ids.ScreenId, scbase.GenWaveUUID())
	runPacket.UsePty = true
	ptermVal := defaultStr(pk.Kwargs["wterm"], DefaultPTERM)
	runPacket.TermOpts, err = GetUITermOpts(pk.UIContext.WinSize, ptermVal)
	if err != nil {
		return nil, fmt.Errorf("/sync error, invalid 'wterm' value %q: %v", ptermVal, err)
	}
	runPacket.Command = ":"
	runPacket.ReturnState = true
	cmd, callback, err := remote.RunCommand(ctx, ids.SessionId, ids.ScreenId, ids.Remote.RemotePtr, runPacket)
	if callback != nil {
		defer callback()
	}
	if err != nil {
		return nil, err
	}
	cmd.RawCmdStr = pk.GetRawStr()
	update, err := addLineForCmd(ctx, "/sync", true, ids, cmd, "terminal", nil)
	if err != nil {
		return nil, err
	}
	update.Interactive = pk.Interactive
	sstore.MainBus.SendScreenUpdate(ids.ScreenId, update)
	return nil, nil
}

func getRendererArg(pk *scpacket.FeCommandPacketType) (string, error) {
	rval := pk.Kwargs[KwArgView]
	if rval == "" {
		rval = pk.Kwargs[KwArgRenderer]
	}
	if rval == "" {
		return "", nil
	}
	err := validateRenderer(rval)
	if err != nil {
		return "", err
	}
	return rval, nil
}

func getTemplateArg(pk *scpacket.FeCommandPacketType) (string, error) {
	rval := pk.Kwargs[KwArgTemplate]
	if rval == "" {
		return "", nil
	}
	// TODO validate
	return rval, nil
}

func getLangArg(pk *scpacket.FeCommandPacketType) (string, error) {
	// TODO better error checking
	if len(pk.Kwargs[KwArgLang]) > 50 {
		return "", nil // TODO return error, don't fail silently
	}
	return pk.Kwargs[KwArgLang], nil
}

func RunCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, fmt.Errorf("/run error: %w", err)
	}
	renderer, err := getRendererArg(pk)
	if err != nil {
		return nil, fmt.Errorf("/run error, invalid view/renderer: %w", err)
	}
	templateArg, err := getTemplateArg(pk)
	if err != nil {
		return nil, fmt.Errorf("/run error, invalid template: %w", err)
	}
	langArg, err := getLangArg(pk)
	if err != nil {
		return nil, fmt.Errorf("/run error, invalid lang: %w", err)
	}
	cmdStr := firstArg(pk)
	expandedCmdStr, err := doCmdHistoryExpansion(ctx, ids, cmdStr)
	if err != nil {
		return nil, err
	}
	if expandedCmdStr != "" {
		newPk := scpacket.MakeFeCommandPacket()
		newPk.MetaCmd = "eval"
		newPk.Args = []string{expandedCmdStr}
		newPk.Kwargs = pk.Kwargs
		newPk.RawStr = pk.RawStr
		newPk.UIContext = pk.UIContext
		newPk.Interactive = pk.Interactive
		evalDepth := getEvalDepth(ctx)
		ctxWithDepth := context.WithValue(ctx, depthContextKey, evalDepth+1)
		return EvalCommand(ctxWithDepth, newPk)
	}
	isRtnStateCmd := IsReturnStateCommand(cmdStr)
	// runPacket.State is set in remote.RunCommand()
	runPacket := packet.MakeRunPacket()
	runPacket.ReqId = uuid.New().String()
	runPacket.CK = base.MakeCommandKey(ids.ScreenId, scbase.GenWaveUUID())
	runPacket.UsePty = true
	ptermVal := defaultStr(pk.Kwargs["wterm"], DefaultPTERM)
	runPacket.TermOpts, err = GetUITermOpts(pk.UIContext.WinSize, ptermVal)
	if err != nil {
		return nil, fmt.Errorf("/run error, invalid 'pterm' value %q: %v", ptermVal, err)
	}
	runPacket.Command = strings.TrimSpace(cmdStr)
	runPacket.ReturnState = resolveBool(pk.Kwargs["rtnstate"], isRtnStateCmd)
	cmd, callback, err := remote.RunCommand(ctx, ids.SessionId, ids.ScreenId, ids.Remote.RemotePtr, runPacket)
	if callback != nil {
		defer callback()
	}
	if err != nil {
		return nil, err
	}
	cmd.RawCmdStr = pk.GetRawStr()
	lineState := make(map[string]any)
	if templateArg != "" {
		lineState[sstore.LineState_Template] = templateArg
	}
	if langArg != "" {
		lineState[sstore.LineState_Lang] = langArg
	}
	update, err := addLineForCmd(ctx, "/run", true, ids, cmd, renderer, lineState)
	if err != nil {
		return nil, err
	}
	update.Interactive = pk.Interactive
	sstore.MainBus.SendScreenUpdate(ids.ScreenId, update)
	return nil, nil
}

func addToHistory(ctx context.Context, pk *scpacket.FeCommandPacketType, historyContext historyContextType, isMetaCmd bool, hadError bool) error {
	cmdStr := firstArg(pk)
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return err
	}
	isIncognito, err := sstore.IsIncognitoScreen(ctx, ids.SessionId, ids.ScreenId)
	if err != nil {
		return fmt.Errorf("cannot add to history, error looking up incognito status of screen: %v", err)
	}
	hitem := &sstore.HistoryItemType{
		HistoryId: scbase.GenWaveUUID(),
		Ts:        time.Now().UnixMilli(),
		UserId:    DefaultUserId,
		SessionId: ids.SessionId,
		ScreenId:  ids.ScreenId,
		LineId:    historyContext.LineId,
		LineNum:   historyContext.LineNum,
		HadError:  hadError,
		CmdStr:    cmdStr,
		IsMetaCmd: isMetaCmd,
		Incognito: isIncognito,
	}
	if !isMetaCmd && historyContext.RemotePtr != nil {
		hitem.Remote = *historyContext.RemotePtr
	}
	err = sstore.InsertHistoryItem(ctx, hitem)
	if err != nil {
		return err
	}
	return nil
}

func EvalCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("usage: /eval [command], no command passed to eval")
	}
	if len(pk.Args[0]) > MaxCommandLen {
		return nil, fmt.Errorf("command length too long len:%d, max:%d", len(pk.Args[0]), MaxCommandLen)
	}
	evalDepth := getEvalDepth(ctx)
	if pk.Interactive && evalDepth == 0 {
		err := sstore.UpdateCurrentActivity(ctx, sstore.ActivityUpdate{NumCommands: 1})
		if err != nil {
			log.Printf("[error] incrementing activity numcommands: %v\n", err)
			// fall through (non-fatal error)
		}
	}
	if evalDepth > MaxEvalDepth {
		return nil, fmt.Errorf("alias/history expansion max-depth exceeded")
	}
	var historyContext historyContextType
	ctxWithHistory := context.WithValue(ctx, historyContextKey, &historyContext)
	var update sstore.UpdatePacket
	newPk, rtnErr := EvalMetaCommand(ctxWithHistory, pk)
	if rtnErr == nil {
		update, rtnErr = HandleCommand(ctxWithHistory, newPk)
	}
	if !resolveBool(pk.Kwargs["nohist"], false) {
		err := addToHistory(ctx, pk, historyContext, (newPk.MetaCmd != "run"), (rtnErr != nil))
		if err != nil {
			log.Printf("[error] adding to history: %v\n", err)
			// fall through (non-fatal error)
		}
	}
	return update, rtnErr
}

func ScreenArchiveCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session) // don't force R_Screen
	if err != nil {
		return nil, fmt.Errorf("/screen:archive cannot archive screen: %w", err)
	}
	screenId := ids.ScreenId
	if len(pk.Args) > 0 {
		ri, err := resolveSessionScreen(ctx, ids.SessionId, pk.Args[0], ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("/screen:archive cannot resolve screen arg: %v", err)
		}
		screenId = ri.Id
	}
	if screenId == "" {
		return nil, fmt.Errorf("/screen:archive no active screen or screen arg passed")
	}
	archiveVal := true
	if len(pk.Args) > 1 {
		archiveVal = resolveBool(pk.Args[1], true)
	}
	var update sstore.UpdatePacket
	if archiveVal {
		update, err = sstore.ArchiveScreen(ctx, ids.SessionId, screenId)
		if err != nil {
			return nil, err
		}
		return update, nil
	} else {
		log.Printf("unarchive screen %s\n", screenId)
		err = sstore.UnArchiveScreen(ctx, ids.SessionId, screenId)
		if err != nil {
			return nil, fmt.Errorf("/screen:archive cannot un-archive screen: %v", err)
		}
		screen, err := sstore.GetScreenById(ctx, screenId)
		if err != nil {
			return nil, fmt.Errorf("/screen:archive cannot get updated screen obj: %v", err)
		}
		update := &sstore.ModelUpdate{
			Screens: []*sstore.ScreenType{screen},
		}
		return update, nil
	}
}

func ScreenPurgeCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session) // don't force R_Screen
	if err != nil {
		return nil, fmt.Errorf("/screen:purge cannot purge screen: %w", err)
	}
	screenId := ids.ScreenId
	if len(pk.Args) > 0 {
		ri, err := resolveSessionScreen(ctx, ids.SessionId, pk.Args[0], ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("/screen:purge cannot resolve screen arg: %v", err)
		}
		screenId = ri.Id
	}
	if screenId == "" {
		return nil, fmt.Errorf("/screen:purge no active screen or screen arg passed")
	}
	update, err := sstore.PurgeScreen(ctx, screenId, false)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func ScreenOpenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, fmt.Errorf("/screen:open cannot open screen: %w", err)
	}
	activate := resolveBool(pk.Kwargs["activate"], true)
	newName := pk.Kwargs["name"]
	if newName != "" {
		err := validateName(newName, "screen")
		if err != nil {
			return nil, err
		}
	}
	update, err := sstore.InsertScreen(ctx, ids.SessionId, newName, sstore.ScreenCreateOpts{}, activate)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func ScreenSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	var varsUpdated []string
	var setNonAnchor bool // anchor does not receive an update
	updateMap := make(map[string]interface{})
	if pk.Kwargs["name"] != "" {
		newName := pk.Kwargs["name"]
		err = validateName(newName, "screen")
		if err != nil {
			return nil, err
		}
		updateMap[sstore.ScreenField_Name] = newName
		varsUpdated = append(varsUpdated, "name")
		setNonAnchor = true
	}
	if pk.Kwargs["sharename"] != "" {
		shareName := pk.Kwargs["sharename"]
		err = validateShareName(shareName)
		if err != nil {
			return nil, err
		}
		updateMap[sstore.ScreenField_ShareName] = shareName
		varsUpdated = append(varsUpdated, "sharename")
		setNonAnchor = true
	}
	if pk.Kwargs["tabcolor"] != "" {
		color := pk.Kwargs["tabcolor"]
		err = validateColor(color, "screen tabcolor")
		if err != nil {
			return nil, err
		}
		updateMap[sstore.ScreenField_TabColor] = color
		varsUpdated = append(varsUpdated, "tabcolor")
		setNonAnchor = true
	}
	if pk.Kwargs["tabicon"] != "" {
		icon := pk.Kwargs["tabicon"]
		updateMap[sstore.ScreenField_TabIcon] = icon
		varsUpdated = append(varsUpdated, "tabicon")
		setNonAnchor = true
	}
	if pk.Kwargs["pos"] != "" {
		varsUpdated = append(varsUpdated, "pos")
		setNonAnchor = true
	}
	if pk.Kwargs["focus"] != "" {
		focusVal := pk.Kwargs["focus"]
		if focusVal != sstore.ScreenFocusInput && focusVal != sstore.ScreenFocusCmd {
			return nil, fmt.Errorf("/screen:set invalid focus argument %q, must be %s", focusVal, formatStrs([]string{sstore.ScreenFocusInput, sstore.ScreenFocusCmd}, "or", false))
		}
		varsUpdated = append(varsUpdated, "focus")
		updateMap[sstore.ScreenField_Focus] = focusVal
		setNonAnchor = true
	}
	if pk.Kwargs["line"] != "" {
		screen, err := sstore.GetScreenById(ctx, ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("/screen:set cannot get screen: %v", err)
		}
		var selectedLineStr string
		if screen.SelectedLine > 0 {
			selectedLineStr = strconv.Itoa(int(screen.SelectedLine))
		}
		ritem, err := resolveLine(ctx, screen.SessionId, screen.ScreenId, pk.Kwargs["line"], selectedLineStr)
		if err != nil {
			return nil, fmt.Errorf("/screen:set error resolving line: %v", err)
		}
		if ritem == nil {
			return nil, fmt.Errorf("/screen:set could not resolve line %q", pk.Kwargs["line"])
		}
		varsUpdated = append(varsUpdated, "line")
		setNonAnchor = true
		updateMap[sstore.ScreenField_SelectedLine] = ritem.Num
	}
	if pk.Kwargs["anchor"] != "" {
		m := screenAnchorRe.FindStringSubmatch(pk.Kwargs["anchor"])
		if m == nil {
			return nil, fmt.Errorf("/screen:set invalid anchor argument (must be [line] or [line]:[offset])")
		}
		anchorLine, _ := strconv.Atoi(m[1])
		varsUpdated = append(varsUpdated, "anchor")
		updateMap[sstore.ScreenField_AnchorLine] = anchorLine
		if m[2] != "" {
			anchorOffset, _ := strconv.Atoi(m[2])
			updateMap[sstore.ScreenField_AnchorOffset] = anchorOffset
		} else {
			updateMap[sstore.ScreenField_AnchorOffset] = 0
		}
	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/screen:set no updates, can set %s", formatStrs([]string{"name", "pos", "tabcolor", "tabicon", "focus", "anchor", "line", "sharename"}, "or", false))
	}
	screen, err := sstore.UpdateScreen(ctx, ids.ScreenId, updateMap)
	if err != nil {
		return nil, fmt.Errorf("error updating screen: %v", err)
	}
	if !setNonAnchor {
		return nil, nil
	}
	update := &sstore.ModelUpdate{
		Screens: []*sstore.ScreenType{screen},
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("screen updated %s", formatStrs(varsUpdated, "and", false)),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func ScreenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, fmt.Errorf("/screen cannot switch to screen: %w", err)
	}
	firstArg := firstArg(pk)
	if firstArg == "" {
		return nil, fmt.Errorf("usage /screen [screen-name|screen-index|screen-id], no param specified")
	}
	ritem, err := resolveSessionScreen(ctx, ids.SessionId, firstArg, ids.ScreenId)
	if err != nil {
		return nil, err
	}
	update, err := sstore.SwitchScreenById(ctx, ids.SessionId, ritem.Id)
	if err != nil {
		return nil, err
	}
	return update, nil
}

var screenAnchorRe = regexp.MustCompile("^(\\d+)(?::(-?\\d+))?$")

func RemoteInstallCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	mshell := ids.Remote.MShell
	go mshell.RunInstall()
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteInstallCancelCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	mshell := ids.Remote.MShell
	go mshell.CancelInstall()
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteConnectCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	go ids.Remote.MShell.Launch(true)
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteDisconnectCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	force := resolveBool(pk.Kwargs["force"], false)
	go ids.Remote.MShell.Disconnect(force)
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func makeRemoteEditUpdate_new(err error) sstore.UpdatePacket {
	redit := &sstore.RemoteEditType{
		RemoteEdit: true,
	}
	if err != nil {
		redit.ErrorStr = err.Error()
	}
	update := &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			RemoteEdit: redit,
		},
	}
	return update
}

func makeRemoteEditErrorReturn_new(visual bool, err error) (sstore.UpdatePacket, error) {
	if visual {
		return makeRemoteEditUpdate_new(err), nil
	}
	return nil, err
}

func makeRemoteEditUpdate_edit(ids resolvedIds, err error) sstore.UpdatePacket {
	redit := &sstore.RemoteEditType{
		RemoteEdit: true,
	}
	redit.RemoteId = ids.Remote.RemotePtr.RemoteId
	if ids.Remote.RemoteCopy.SSHOpts != nil {
		redit.KeyStr = ids.Remote.RemoteCopy.SSHOpts.SSHIdentity
		redit.HasPassword = (ids.Remote.RemoteCopy.SSHOpts.SSHPassword != "")
	}
	if err != nil {
		redit.ErrorStr = err.Error()
	}
	update := &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			RemoteEdit: redit,
		},
	}
	return update
}

func makeRemoteEditErrorReturn_edit(ids resolvedIds, visual bool, err error) (sstore.UpdatePacket, error) {
	if visual {
		return makeRemoteEditUpdate_edit(ids, err), nil
	}
	return nil, err
}

type RemoteEditArgs struct {
	CanonicalName string
	SSHOpts       *sstore.SSHOpts
	ConnectMode   string
	Alias         string
	AutoInstall   bool
	SSHPassword   string
	SSHKeyFile    string
	Color         string
	EditMap       map[string]interface{}
}

func parseRemoteEditArgs(isNew bool, pk *scpacket.FeCommandPacketType, isLocal bool) (*RemoteEditArgs, error) {
	var canonicalName string
	var sshOpts *sstore.SSHOpts
	var isSudo bool

	if isNew {
		if len(pk.Args) == 0 {
			return nil, fmt.Errorf("/remote:new must specify user@host argument (set visual=1 to edit in UI)")
		}
		userHost := pk.Args[0]
		m := userHostRe.FindStringSubmatch(userHost)
		if m == nil {
			return nil, fmt.Errorf("invalid format of user@host argument")
		}
		sudoStr, remoteUser, remoteHost, remotePortStr := m[1], m[2], m[3], m[4]
		var uhPort int
		if remotePortStr != "" {
			var err error
			uhPort, err = strconv.Atoi(remotePortStr)
			if err != nil {
				return nil, fmt.Errorf("invalid port specified on user@host argument")
			}
		}
		if sudoStr != "" {
			isSudo = true
		}
		if pk.Kwargs["sudo"] != "" {
			sudoArg := resolveBool(pk.Kwargs["sudo"], false)
			if isSudo && !sudoArg {
				return nil, fmt.Errorf("invalid 'sudo' argument, with sudo kw arg set to false")
			}
			if !isSudo && sudoArg {
				isSudo = true
			}
		}
		sshOpts = &sstore.SSHOpts{
			Local:   false,
			SSHHost: remoteHost,
			SSHUser: remoteUser,
			IsSudo:  isSudo,
		}
		portVal, err := resolvePosInt(pk.Kwargs["port"], 0)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %v", pk.Kwargs["port"], err)
		}
		if portVal != 0 && uhPort != 0 && portVal != uhPort {
			return nil, fmt.Errorf("invalid port argument, does not match port specified in 'user@host:port' argument")
		}
		if portVal == 0 && uhPort != 0 {
			portVal = uhPort
		}
		sshOpts.SSHPort = portVal
		canonicalName = remoteUser + "@" + remoteHost
		if isSudo {
			canonicalName = "sudo@" + canonicalName
		}
	} else {
		if pk.Kwargs["sudo"] != "" {
			return nil, fmt.Errorf("cannot update 'sudo' value")
		}
		if pk.Kwargs["port"] != "" {
			return nil, fmt.Errorf("cannot update 'port' value")
		}
	}
	alias := pk.Kwargs["alias"]
	if alias != "" {
		if len(alias) > MaxRemoteAliasLen {
			return nil, fmt.Errorf("alias too long, max length = %d", MaxRemoteAliasLen)
		}
		if !remoteAliasRe.MatchString(alias) {
			return nil, fmt.Errorf("invalid alias format")
		}
	}
	var connectMode string
	if isNew {
		connectMode = sstore.ConnectModeAuto
	}
	if pk.Kwargs["connectmode"] != "" {
		connectMode = pk.Kwargs["connectmode"]
	}
	if connectMode != "" && !sstore.IsValidConnectMode(connectMode) {
		err := fmt.Errorf("invalid connectmode %q: valid modes are %s", connectMode, formatStrs([]string{sstore.ConnectModeStartup, sstore.ConnectModeAuto, sstore.ConnectModeManual}, "or", false))
		return nil, err
	}
	keyFile, err := resolveFile(pk.Kwargs["key"])
	if err != nil {
		return nil, fmt.Errorf("invalid ssh keyfile %q: %v", pk.Kwargs["key"], err)
	}
	color := pk.Kwargs["color"]
	if color != "" {
		err := validateRemoteColor(color, "remote color")
		if err != nil {
			return nil, err
		}
	}
	sshPassword := pk.Kwargs["password"]
	if sshOpts != nil {
		sshOpts.SSHIdentity = keyFile
		sshOpts.SSHPassword = sshPassword
	}

	// set up editmap
	editMap := make(map[string]interface{})
	if _, found := pk.Kwargs[sstore.RemoteField_Alias]; found {
		editMap[sstore.RemoteField_Alias] = alias
	}
	if connectMode != "" {
		if isLocal {
			return nil, fmt.Errorf("Cannot edit connect mode for 'local' remote")
		}
		editMap[sstore.RemoteField_ConnectMode] = connectMode
	}
	if _, found := pk.Kwargs["key"]; found {
		if isLocal {
			return nil, fmt.Errorf("Cannot edit ssh key file for 'local' remote")
		}
		editMap[sstore.RemoteField_SSHKey] = keyFile
	}
	if _, found := pk.Kwargs[sstore.RemoteField_Color]; found {
		editMap[sstore.RemoteField_Color] = color
	}
	if _, found := pk.Kwargs["password"]; found && pk.Kwargs["password"] != PasswordUnchangedSentinel {
		if isLocal {
			return nil, fmt.Errorf("Cannot edit ssh password for 'local' remote")
		}
		editMap[sstore.RemoteField_SSHPassword] = sshPassword
	}

	return &RemoteEditArgs{
		SSHOpts:       sshOpts,
		ConnectMode:   connectMode,
		Alias:         alias,
		AutoInstall:   true,
		CanonicalName: canonicalName,
		SSHKeyFile:    keyFile,
		SSHPassword:   sshPassword,
		Color:         color,
		EditMap:       editMap,
	}, nil
}

func RemoteNewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	visualEdit := resolveBool(pk.Kwargs["visual"], false)
	isSubmitted := resolveBool(pk.Kwargs["submit"], false)
	if visualEdit && !isSubmitted && len(pk.Args) == 0 {
		return makeRemoteEditUpdate_new(nil), nil
	}
	editArgs, err := parseRemoteEditArgs(true, pk, false)
	if err != nil {
		return nil, fmt.Errorf("/remote:new %v", err)
	}
	r := &sstore.RemoteType{
		RemoteId:            scbase.GenWaveUUID(),
		RemoteType:          sstore.RemoteTypeSsh,
		RemoteAlias:         editArgs.Alias,
		RemoteCanonicalName: editArgs.CanonicalName,
		RemoteUser:          editArgs.SSHOpts.SSHUser,
		RemoteHost:          editArgs.SSHOpts.SSHHost,
		ConnectMode:         editArgs.ConnectMode,
		AutoInstall:         editArgs.AutoInstall,
		SSHOpts:             editArgs.SSHOpts,
	}
	if editArgs.Color != "" {
		r.RemoteOpts = &sstore.RemoteOptsType{Color: editArgs.Color}
	}
	err = remote.AddRemote(ctx, r, true)
	if err != nil {
		return nil, fmt.Errorf("cannot create remote %q: %v", r.RemoteCanonicalName, err)
	}
	// SUCCESS
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: r.RemoteId,
		},
	}, nil
}

func RemoteSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	visualEdit := resolveBool(pk.Kwargs["visual"], false)
	isSubmitted := resolveBool(pk.Kwargs["submit"], false)
	editArgs, err := parseRemoteEditArgs(false, pk, ids.Remote.MShell.IsLocal())
	if err != nil {
		return makeRemoteEditErrorReturn_edit(ids, visualEdit, fmt.Errorf("/remote:new %v", err))
	}
	if visualEdit && !isSubmitted && len(editArgs.EditMap) == 0 {
		return makeRemoteEditUpdate_edit(ids, nil), nil
	}
	if !visualEdit && len(editArgs.EditMap) == 0 {
		return nil, fmt.Errorf("/remote:set no updates, can set %s.  (set visual=1 to edit in UI)", formatStrs(RemoteSetArgs, "or", false))
	}
	err = ids.Remote.MShell.UpdateRemote(ctx, editArgs.EditMap)
	if err != nil {
		return makeRemoteEditErrorReturn_edit(ids, visualEdit, fmt.Errorf("/remote:new error updating remote: %v", err))
	}
	if visualEdit {
		return &sstore.ModelUpdate{
			RemoteView: &sstore.RemoteViewType{
				PtyRemoteId: ids.Remote.RemoteCopy.RemoteId,
			},
		}, nil
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("remote %q updated", ids.Remote.DisplayName),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func RemoteShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	state := ids.Remote.RState
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			PtyRemoteId: state.RemoteId,
		},
	}, nil
}

func RemoteShowAllCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	stateArr := remote.GetAllRemoteRuntimeState()
	var buf bytes.Buffer
	for _, rstate := range stateArr {
		var name string
		if rstate.RemoteAlias == "" {
			name = rstate.RemoteCanonicalName
		} else {
			name = fmt.Sprintf("%s (%s)", rstate.RemoteCanonicalName, rstate.RemoteAlias)
		}
		buf.WriteString(fmt.Sprintf("%-12s %-5s %8s  %s\n", rstate.Status, rstate.RemoteType, rstate.RemoteId[0:8], name))
	}
	return &sstore.ModelUpdate{
		RemoteView: &sstore.RemoteViewType{
			RemoteShowAll: true,
		},
	}, nil
}

func ScreenShowAllCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	screenArr, err := sstore.GetSessionScreens(ctx, ids.SessionId)
	if err != nil {
		return nil, fmt.Errorf("/screen:showall error getting screen list: %v", err)
	}
	var buf bytes.Buffer
	for _, screen := range screenArr {
		var archivedStr string
		if screen.Archived {
			archivedStr = " (archived)"
		}
		screenIdxStr := "-"
		if screen.ScreenIdx != 0 {
			screenIdxStr = strconv.Itoa(int(screen.ScreenIdx))
		}
		outStr := fmt.Sprintf("%-30s %s  %s\n", screen.Name+archivedStr, screen.ScreenId, screenIdxStr)
		buf.WriteString(outStr)
	}
	return &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("all screens for session"),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}, nil
}

func ScreenResetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	localRemote := remote.GetLocalRemote()
	if localRemote == nil {
		return nil, fmt.Errorf("error getting local remote (not found)")
	}
	rptr := sstore.RemotePtrType{RemoteId: localRemote.RemoteId}
	sessionUpdate := &sstore.SessionType{SessionId: ids.SessionId}
	ris, err := sstore.ScreenReset(ctx, ids.ScreenId)
	if err != nil {
		return nil, fmt.Errorf("error resetting screen: %v", err)
	}
	sessionUpdate.Remotes = append(sessionUpdate.Remotes, ris...)
	err = sstore.UpdateCurRemote(ctx, ids.ScreenId, rptr)
	if err != nil {
		return nil, fmt.Errorf("cannot reset screen remote back to local: %w", err)
	}
	outputStr := "reset screen state (all remote state reset)"
	cmd, err := makeStaticCmd(ctx, "screen:reset", ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/screen:reset", false, ids, cmd, "", nil)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	update.Sessions = []*sstore.SessionType{sessionUpdate}
	return update, nil
}

func RemoteArchiveCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	err = remote.ArchiveRemote(ctx, ids.Remote.RemotePtr.RemoteId)
	if err != nil {
		return nil, fmt.Errorf("archiving remote: %v", err)
	}
	update := sstore.InfoMsgUpdate("remote [%s] archived", ids.Remote.DisplayName)
	localRemote := remote.GetLocalRemote()
	rptr := sstore.RemotePtrType{RemoteId: localRemote.GetRemoteId()}
	err = sstore.UpdateCurRemote(ctx, ids.ScreenId, rptr)
	if err != nil {
		return nil, fmt.Errorf("cannot switch remote back to local: %w", err)
	}
	screen, err := sstore.GetScreenById(ctx, ids.ScreenId)
	if err != nil {
		return nil, fmt.Errorf("cannot get updated screen: %w", err)
	}
	update.Screens = []*sstore.ScreenType{screen}
	return update, nil
}

func RemoteCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/remote requires a subcommand: %s", formatStrs([]string{"show"}, "or", false))
}

func crShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType, ids resolvedIds) (sstore.UpdatePacket, error) {
	var buf bytes.Buffer
	riArr, err := sstore.GetRIsForScreen(ctx, ids.SessionId, ids.ScreenId)
	if err != nil {
		return nil, fmt.Errorf("cannot get remote instances: %w", err)
	}
	rmap := remote.GetRemoteMap()
	for _, ri := range riArr {
		rptr := sstore.RemotePtrType{RemoteId: ri.RemoteId, Name: ri.Name}
		msh := rmap[ri.RemoteId]
		if msh == nil {
			continue
		}
		baseDisplayName := msh.GetDisplayName()
		displayName := rptr.GetDisplayName(baseDisplayName)
		cwdStr := "-"
		if ri.FeState["cwd"] != "" {
			cwdStr = ri.FeState["cwd"]
		}
		buf.WriteString(fmt.Sprintf("%-30s %-50s\n", displayName, cwdStr))
	}
	riBaseMap := make(map[string]bool)
	for _, ri := range riArr {
		if ri.Name == "" {
			riBaseMap[ri.RemoteId] = true
		}
	}
	for remoteId, msh := range rmap {
		if riBaseMap[remoteId] {
			continue
		}
		feState := msh.GetDefaultFeState()
		if feState == nil {
			continue
		}
		cwdStr := "-"
		if feState["cwd"] != "" {
			cwdStr = feState["cwd"]
		}
		buf.WriteString(fmt.Sprintf("%-30s %-50s (default)\n", msh.GetDisplayName(), cwdStr))
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func GetFullRemoteDisplayName(rptr *sstore.RemotePtrType, rstate *remote.RemoteRuntimeState) string {
	if rptr == nil {
		return "(invalid)"
	}
	if rstate.RemoteAlias != "" {
		fullName := rstate.RemoteAlias
		if rptr.Name != "" {
			fullName = fullName + ":" + rptr.Name
		}
		return fmt.Sprintf("[%s] (%s)", fullName, rstate.RemoteCanonicalName)
	} else {
		if rptr.Name != "" {
			return fmt.Sprintf("[%s:%s]", rstate.RemoteCanonicalName, rptr.Name)
		}
		return fmt.Sprintf("[%s]", rstate.RemoteCanonicalName)
	}
}

func writeErrorToPty(cmd *sstore.CmdType, errStr string, outputPos int64) {
	errPk := openai.CreateErrorPacket(errStr)
	errBytes, err := packet.MarshalPacket(errPk)
	if err != nil {
		log.Printf("error writing error packet to openai response: %v\n", err)
		return
	}
	errCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFn()
	update, err := sstore.AppendToCmdPtyBlob(errCtx, cmd.ScreenId, cmd.LineId, errBytes, outputPos)
	if err != nil {
		log.Printf("error writing ptyupdate for openai response: %v\n", err)
		return
	}
	sstore.MainBus.SendScreenUpdate(cmd.ScreenId, update)
	return
}

func writePacketToPty(ctx context.Context, cmd *sstore.CmdType, pk packet.PacketType, outputPos *int64) error {
	outBytes, err := packet.MarshalPacket(pk)
	if err != nil {
		return err
	}
	update, err := sstore.AppendToCmdPtyBlob(ctx, cmd.ScreenId, cmd.LineId, outBytes, *outputPos)
	if err != nil {
		return err
	}
	*outputPos += int64(len(outBytes))
	sstore.MainBus.SendScreenUpdate(cmd.ScreenId, update)
	return nil
}

func doOpenAICompletion(cmd *sstore.CmdType, opts *sstore.OpenAIOptsType, prompt []sstore.OpenAIPromptMessageType) {
	var outputPos int64
	var hadError bool
	startTime := time.Now()
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()
	defer func() {
		r := recover()
		if r != nil {
			panicMsg := fmt.Sprintf("panic: %v", r)
			log.Printf("panic in doOpenAICompletion: %s\n", panicMsg)
			writeErrorToPty(cmd, panicMsg, outputPos)
			hadError = true
		}
		duration := time.Since(startTime)
		cmdStatus := sstore.CmdStatusDone
		var exitCode int
		if hadError {
			cmdStatus = sstore.CmdStatusError
			exitCode = 1
		}
		ck := base.MakeCommandKey(cmd.ScreenId, cmd.LineId)
		donePk := packet.MakeCmdDonePacket(ck)
		donePk.Ts = time.Now().UnixMilli()
		donePk.ExitCode = exitCode
		donePk.DurationMs = duration.Milliseconds()
		update, err := sstore.UpdateCmdDoneInfo(context.Background(), ck, donePk, cmdStatus)
		if err != nil {
			// nothing to do
			log.Printf("error updating cmddoneinfo (in openai): %v\n", err)
			return
		}
		sstore.MainBus.SendScreenUpdate(cmd.ScreenId, update)
	}()
	respPks, err := openai.RunCompletion(ctx, opts, prompt)
	if err != nil {
		writeErrorToPty(cmd, fmt.Sprintf("error calling OpenAI API: %v", err), outputPos)
		return
	}
	for _, pk := range respPks {
		err = writePacketToPty(ctx, cmd, pk, &outputPos)
		if err != nil {
			writeErrorToPty(cmd, fmt.Sprintf("error writing response to ptybuffer: %v", err), outputPos)
			return
		}
	}
	return
}

func doOpenAIStreamCompletion(cmd *sstore.CmdType, opts *sstore.OpenAIOptsType, prompt []sstore.OpenAIPromptMessageType) {
	var outputPos int64
	var hadError bool
	startTime := time.Now()
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()
	defer func() {
		r := recover()
		if r != nil {
			panicMsg := fmt.Sprintf("panic: %v", r)
			log.Printf("panic in doOpenAICompletion: %s\n", panicMsg)
			writeErrorToPty(cmd, panicMsg, outputPos)
			hadError = true
		}
		duration := time.Since(startTime)
		cmdStatus := sstore.CmdStatusDone
		var exitCode int
		if hadError {
			cmdStatus = sstore.CmdStatusError
			exitCode = 1
		}
		ck := base.MakeCommandKey(cmd.ScreenId, cmd.LineId)
		donePk := packet.MakeCmdDonePacket(ck)
		donePk.Ts = time.Now().UnixMilli()
		donePk.ExitCode = exitCode
		donePk.DurationMs = duration.Milliseconds()
		update, err := sstore.UpdateCmdDoneInfo(context.Background(), ck, donePk, cmdStatus)
		if err != nil {
			// nothing to do
			log.Printf("error updating cmddoneinfo (in openai): %v\n", err)
			return
		}
		sstore.MainBus.SendScreenUpdate(cmd.ScreenId, update)
	}()
	ch, err := openai.RunCompletionStream(ctx, opts, prompt)
	if err != nil {
		writeErrorToPty(cmd, fmt.Sprintf("error calling OpenAI API: %v", err), outputPos)
		return
	}
	for pk := range ch {
		err = writePacketToPty(ctx, cmd, pk, &outputPos)
		if err != nil {
			writeErrorToPty(cmd, fmt.Sprintf("error writing response to ptybuffer: %v", err), outputPos)
			return
		}
	}
	return
}

func OpenAICommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, fmt.Errorf("/%s error: %w", GetCmdStr(pk), err)
	}
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	if clientData.OpenAIOpts == nil || clientData.OpenAIOpts.APIToken == "" {
		return nil, fmt.Errorf("no openai API token found, configure in client settings")
	}
	opts := clientData.OpenAIOpts
	if opts.Model == "" {
		opts.Model = openai.DefaultModel
	}
	if opts.MaxTokens == 0 {
		opts.MaxTokens = openai.DefaultMaxTokens
	}
	promptStr := firstArg(pk)
	if promptStr == "" {
		return nil, fmt.Errorf("openai error, prompt string is blank")
	}
	ptermVal := defaultStr(pk.Kwargs["wterm"], DefaultPTERM)
	pkTermOpts, err := GetUITermOpts(pk.UIContext.WinSize, ptermVal)
	if err != nil {
		return nil, fmt.Errorf("openai error, invalid 'pterm' value %q: %v", ptermVal, err)
	}
	termOpts := convertTermOpts(pkTermOpts)
	cmd, err := makeDynCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), *termOpts)
	if err != nil {
		return nil, fmt.Errorf("openai error, cannot make dyn cmd")
	}
	line, err := sstore.AddOpenAILine(ctx, ids.ScreenId, DefaultUserId, cmd)
	if err != nil {
		return nil, fmt.Errorf("cannot add new line: %v", err)
	}
	prompt := []sstore.OpenAIPromptMessageType{{Role: sstore.OpenAIRoleUser, Content: promptStr}}
	if resolveBool(pk.Kwargs["stream"], true) {
		go doOpenAIStreamCompletion(cmd, opts, prompt)
	} else {
		go doOpenAICompletion(cmd, opts, prompt)
	}
	updateHistoryContext(ctx, line, cmd)
	updateMap := make(map[string]interface{})
	updateMap[sstore.ScreenField_SelectedLine] = line.LineNum
	updateMap[sstore.ScreenField_Focus] = sstore.ScreenFocusInput
	screen, err := sstore.UpdateScreen(ctx, ids.ScreenId, updateMap)
	if err != nil {
		// ignore error again (nothing to do)
		log.Printf("openai error updating screen selected line: %v\n", err)
	}
	update := &sstore.ModelUpdate{Line: line, Cmd: cmd, Screens: []*sstore.ScreenType{screen}}
	return update, nil
}

func CrCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, fmt.Errorf("/%s error: %w", GetCmdStr(pk), err)
	}
	newRemote := firstArg(pk)
	if newRemote == "" {
		return crShowCommand(ctx, pk, ids)
	}
	_, rptr, rstate, err := resolveRemote(ctx, newRemote, ids.SessionId, ids.ScreenId)
	if err != nil {
		return nil, err
	}
	if rptr == nil {
		return nil, fmt.Errorf("/%s error: remote %q not found", GetCmdStr(pk), newRemote)
	}
	if rstate.Archived {
		return nil, fmt.Errorf("/%s error: remote %q cannot switch to archived remote", GetCmdStr(pk), newRemote)
	}
	err = sstore.UpdateCurRemote(ctx, ids.ScreenId, *rptr)
	if err != nil {
		return nil, fmt.Errorf("/%s error: cannot update curremote: %w", GetCmdStr(pk), err)
	}
	noHist := resolveBool(pk.Kwargs["nohist"], false)
	if noHist {
		screen, err := sstore.GetScreenById(ctx, ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("/% error: cannot resolve screen for update: %w", err)
		}
		update := &sstore.ModelUpdate{
			Screens:     []*sstore.ScreenType{screen},
			Interactive: pk.Interactive,
		}
		return update, nil
	}
	outputStr := fmt.Sprintf("connected to %s", GetFullRemoteDisplayName(rptr, rstate))
	cmd, err := makeStaticCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/"+GetCmdStr(pk), false, ids, cmd, "", nil)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	return update, nil
}

func makeDynCmd(ctx context.Context, metaCmd string, ids resolvedIds, cmdStr string, termOpts sstore.TermOpts) (*sstore.CmdType, error) {
	cmd := &sstore.CmdType{
		ScreenId:  ids.ScreenId,
		LineId:    scbase.GenWaveUUID(),
		CmdStr:    cmdStr,
		RawCmdStr: cmdStr,
		Remote:    ids.Remote.RemotePtr,
		TermOpts:  termOpts,
		Status:    sstore.CmdStatusRunning,
		RunOut:    nil,
	}
	if ids.Remote.StatePtr != nil {
		cmd.StatePtr = *ids.Remote.StatePtr
	}
	if ids.Remote.FeState != nil {
		cmd.FeState = ids.Remote.FeState
	}
	err := sstore.CreateCmdPtyFile(ctx, cmd.ScreenId, cmd.LineId, cmd.TermOpts.MaxPtySize)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, fmt.Errorf("cannot create local ptyout file for %s command: %w", metaCmd, err)
	}
	return cmd, nil
}

func makeStaticCmd(ctx context.Context, metaCmd string, ids resolvedIds, cmdStr string, cmdOutput []byte) (*sstore.CmdType, error) {
	cmd := &sstore.CmdType{
		ScreenId:  ids.ScreenId,
		LineId:    scbase.GenWaveUUID(),
		CmdStr:    cmdStr,
		RawCmdStr: cmdStr,
		Remote:    ids.Remote.RemotePtr,
		TermOpts:  sstore.TermOpts{Rows: shexec.DefaultTermRows, Cols: shexec.DefaultTermCols, FlexRows: true, MaxPtySize: remote.DefaultMaxPtySize},
		Status:    sstore.CmdStatusDone,
		RunOut:    nil,
	}
	if ids.Remote.StatePtr != nil {
		cmd.StatePtr = *ids.Remote.StatePtr
	}
	if ids.Remote.FeState != nil {
		cmd.FeState = ids.Remote.FeState
	}
	err := sstore.CreateCmdPtyFile(ctx, cmd.ScreenId, cmd.LineId, cmd.TermOpts.MaxPtySize)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, fmt.Errorf("cannot create local ptyout file for %s command: %w", metaCmd, err)
	}
	// can ignore ptyupdate
	_, err = sstore.AppendToCmdPtyBlob(ctx, ids.ScreenId, cmd.LineId, cmdOutput, 0)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, fmt.Errorf("cannot append to local ptyout file for %s command: %v", metaCmd, err)
	}
	return cmd, nil
}

func addLineForCmd(ctx context.Context, metaCmd string, shouldFocus bool, ids resolvedIds, cmd *sstore.CmdType, renderer string, lineState map[string]any) (*sstore.ModelUpdate, error) {
	rtnLine, err := sstore.AddCmdLine(ctx, ids.ScreenId, DefaultUserId, cmd, renderer, lineState)
	if err != nil {
		return nil, err
	}
	screen, err := sstore.GetScreenById(ctx, ids.ScreenId)
	if err != nil {
		// ignore error here, because the command has already run (nothing to do)
		log.Printf("%s error getting screen: %v\n", metaCmd, err)
	}
	if screen != nil {
		updateMap := make(map[string]interface{})
		updateMap[sstore.ScreenField_SelectedLine] = rtnLine.LineNum
		if shouldFocus {
			updateMap[sstore.ScreenField_Focus] = sstore.ScreenFocusCmd
		}
		screen, err = sstore.UpdateScreen(ctx, ids.ScreenId, updateMap)
		if err != nil {
			// ignore error again (nothing to do)
			log.Printf("%s error updating screen selected line: %v\n", metaCmd, err)
		}
	}
	update := &sstore.ModelUpdate{
		Line:    rtnLine,
		Cmd:     cmd,
		Screens: []*sstore.ScreenType{screen},
	}
	updateHistoryContext(ctx, rtnLine, cmd)
	return update, nil
}

func updateHistoryContext(ctx context.Context, line *sstore.LineType, cmd *sstore.CmdType) {
	ctxVal := ctx.Value(historyContextKey)
	if ctxVal == nil {
		return
	}
	hctx := ctxVal.(*historyContextType)
	if line != nil {
		hctx.LineId = line.LineId
		hctx.LineNum = line.LineNum
	}
	if cmd != nil {
		hctx.RemotePtr = &cmd.Remote
	}
}

func makeInfoFromComps(compType string, comps []string, hasMore bool) sstore.UpdatePacket {
	sort.Slice(comps, func(i int, j int) bool {
		c1 := comps[i]
		c2 := comps[j]
		c1mc := strings.HasPrefix(c1, "^")
		c2mc := strings.HasPrefix(c2, "^")
		if c1mc && !c2mc {
			return true
		}
		if !c1mc && c2mc {
			return false
		}
		return c1 < c2
	})
	if len(comps) == 0 {
		comps = []string{"(no completions)"}
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle:     fmt.Sprintf("%s completions", compType),
			InfoComps:     comps,
			InfoCompsMore: hasMore,
		},
	}
	return update
}

func simpleCompCommandMeta(ctx context.Context, prefix string, compCtx comp.CompContext, args []interface{}) (*comp.CompReturn, error) {
	if strings.HasPrefix(prefix, "/") {
		compsCmd, _ := comp.DoSimpleComp(ctx, comp.CGTypeCommand, prefix, compCtx, nil)
		compsMeta, _ := simpleCompMeta(ctx, prefix, compCtx, nil)
		return comp.CombineCompReturn(comp.CGTypeCommandMeta, compsCmd, compsMeta), nil
	} else {
		compsCmd, _ := comp.DoSimpleComp(ctx, comp.CGTypeCommand, prefix, compCtx, nil)
		compsBareCmd, _ := simpleCompBareCmds(ctx, prefix, compCtx, nil)
		return comp.CombineCompReturn(comp.CGTypeCommand, compsCmd, compsBareCmd), nil
	}
}

func simpleCompBareCmds(ctx context.Context, prefix string, compCtx comp.CompContext, args []interface{}) (*comp.CompReturn, error) {
	rtn := comp.CompReturn{}
	for _, bmc := range BareMetaCmds {
		if strings.HasPrefix(bmc.CmdStr, prefix) {
			rtn.Entries = append(rtn.Entries, comp.CompEntry{Word: bmc.CmdStr, IsMetaCmd: true})
		}
	}
	return &rtn, nil
}

func simpleCompMeta(ctx context.Context, prefix string, compCtx comp.CompContext, args []interface{}) (*comp.CompReturn, error) {
	rtn := comp.CompReturn{}
	validCommands := getValidCommands()
	for _, cmd := range validCommands {
		if strings.HasPrefix(cmd, "/_") && !strings.HasPrefix(prefix, "/_") {
			continue
		}
		if strings.HasPrefix(cmd, prefix) {
			rtn.Entries = append(rtn.Entries, comp.CompEntry{Word: cmd, IsMetaCmd: true})
		}
	}
	return &rtn, nil
}

func doMetaCompGen(ctx context.Context, pk *scpacket.FeCommandPacketType, prefix string, forDisplay bool) ([]string, bool, error) {
	ids, err := resolveUiIds(ctx, pk, 0) // best effort
	var comps []string
	var hasMore bool
	if ids.Remote != nil && ids.Remote.RState.IsConnected() {
		comps, hasMore, err = doCompGen(ctx, pk, prefix, "file", forDisplay)
		if err != nil {
			return nil, false, err
		}
	}
	validCommands := getValidCommands()
	for _, cmd := range validCommands {
		if strings.HasPrefix(cmd, prefix) {
			if forDisplay {
				comps = append(comps, "^"+cmd)
			} else {
				comps = append(comps, cmd)
			}
		}
	}
	return comps, hasMore, nil
}

func doCompGen(ctx context.Context, pk *scpacket.FeCommandPacketType, prefix string, compType string, forDisplay bool) ([]string, bool, error) {
	if compType == "metacommand" {
		return doMetaCompGen(ctx, pk, prefix, forDisplay)
	}
	if !packet.IsValidCompGenType(compType) {
		return nil, false, fmt.Errorf("/_compgen invalid type '%s'", compType)
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, false, fmt.Errorf("/_compgen error: %w", err)
	}
	cgPacket := packet.MakeCompGenPacket()
	cgPacket.ReqId = uuid.New().String()
	cgPacket.CompType = compType
	cgPacket.Prefix = prefix
	cgPacket.Cwd = ids.Remote.FeState["cwd"]
	resp, err := ids.Remote.MShell.PacketRpc(ctx, cgPacket)
	if err != nil {
		return nil, false, err
	}
	if err = resp.Err(); err != nil {
		return nil, false, err
	}
	comps := utilfn.GetStrArr(resp.Data, "comps")
	hasMore := utilfn.GetBool(resp.Data, "hasmore")
	return comps, hasMore, nil
}

func CompGenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, 0) // best-effort
	if err != nil {
		return nil, fmt.Errorf("/_compgen error: %w", err)
	}
	cmdLine := firstArg(pk)
	pos := len(cmdLine)
	if pk.Kwargs["comppos"] != "" {
		posArg, err := strconv.Atoi(pk.Kwargs["comppos"])
		if err != nil {
			return nil, fmt.Errorf("/_compgen invalid comppos '%s': %w", pk.Kwargs["comppos"], err)
		}
		pos = posArg
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(cmdLine) {
		pos = len(cmdLine)
	}
	showComps := resolveBool(pk.Kwargs["compshow"], false)
	cmdSP := utilfn.StrWithPos{Str: cmdLine, Pos: pos}
	compCtx := comp.CompContext{}
	if ids.Remote != nil {
		rptr := ids.Remote.RemotePtr
		compCtx.RemotePtr = &rptr
		if ids.Remote.FeState != nil {
			compCtx.Cwd = ids.Remote.FeState["cwd"]
		}
	}
	compCtx.ForDisplay = showComps
	crtn, newSP, err := comp.DoCompGen(ctx, cmdSP, compCtx)
	if err != nil {
		return nil, err
	}
	if crtn == nil {
		return nil, nil
	}
	if showComps {
		compStrs := crtn.GetCompDisplayStrs()
		return makeInfoFromComps(crtn.CompType, compStrs, crtn.HasMore), nil
	}
	if newSP == nil || cmdSP == *newSP {
		return nil, nil
	}
	update := &sstore.ModelUpdate{
		CmdLine: &sstore.CmdLineType{CmdLine: newSP.Str, CursorPos: newSP.Pos},
	}
	return update, nil
}

func CommentCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, fmt.Errorf("/comment error: %w", err)
	}
	text := firstArg(pk)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("cannot post empty comment")
	}
	rtnLine, err := sstore.AddCommentLine(ctx, ids.ScreenId, DefaultUserId, text)
	if err != nil {
		return nil, err
	}
	updateHistoryContext(ctx, rtnLine, nil)
	updateMap := make(map[string]interface{})
	updateMap[sstore.ScreenField_SelectedLine] = rtnLine.LineNum
	updateMap[sstore.ScreenField_Focus] = sstore.ScreenFocusInput
	screen, err := sstore.UpdateScreen(ctx, ids.ScreenId, updateMap)
	if err != nil {
		// ignore error again (nothing to do)
		log.Printf("/comment error updating screen selected line: %v\n", err)
	}
	update := &sstore.ModelUpdate{Line: rtnLine, Screens: []*sstore.ScreenType{screen}}
	return update, nil
}

func maybeQuote(s string, quote bool) string {
	if quote {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func mapToStrs(m map[string]bool) []string {
	var rtn []string
	for key, val := range m {
		if val {
			rtn = append(rtn, key)
		}
	}
	return rtn
}

func formatStrs(strs []string, conj string, quote bool) string {
	if len(strs) == 0 {
		return "(none)"
	}
	if len(strs) == 1 {
		return maybeQuote(strs[0], quote)
	}
	if len(strs) == 2 {
		return fmt.Sprintf("%s %s %s", maybeQuote(strs[0], quote), conj, maybeQuote(strs[1], quote))
	}
	var buf bytes.Buffer
	for idx := 0; idx < len(strs)-1; idx++ {
		buf.WriteString(maybeQuote(strs[idx], quote))
		buf.WriteString(", ")
	}
	buf.WriteString(conj)
	buf.WriteString(" ")
	buf.WriteString(maybeQuote(strs[len(strs)-1], quote))
	return buf.String()
}

func validateName(name string, typeStr string) error {
	if len(name) > MaxNameLen {
		return fmt.Errorf("%s name too long, max length is %d", typeStr, MaxNameLen)
	}
	if !genericNameRe.MatchString(name) {
		return fmt.Errorf("invalid %s name", typeStr)
	}
	return nil
}

func validateShareName(name string) error {
	if len(name) > MaxShareNameLen {
		return fmt.Errorf("share name too long, max length is %d", MaxShareNameLen)
	}
	for _, ch := range name {
		if !unicode.IsPrint(ch) {
			return fmt.Errorf("invalid character %q in share name", string(ch))
		}
	}
	return nil
}

func validateRenderer(renderer string) error {
	if renderer == "" {
		return nil
	}
	if len(renderer) > MaxRendererLen {
		return fmt.Errorf("renderer name too long, max length is %d", MaxRendererLen)
	}
	if !rendererRe.MatchString(renderer) {
		return fmt.Errorf("invalid renderer format")
	}
	return nil
}

func validateColor(color string, typeStr string) error {
	for _, c := range ColorNames {
		if color == c {
			return nil
		}
	}
	return fmt.Errorf("invalid %s, valid colors are: %s", typeStr, formatStrs(ColorNames, "or", false))
}

func validateRemoteColor(color string, typeStr string) error {
	for _, c := range RemoteColorNames {
		if color == c {
			return nil
		}
	}
	return fmt.Errorf("invalid %s, valid colors are: %s", typeStr, formatStrs(RemoteColorNames, "or", false))
}

func SessionOpenSharedCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	activity := sstore.ActivityUpdate{ClickShared: 1}
	err := sstore.UpdateCurrentActivity(ctx, activity)
	if err != nil {
		log.Printf("error updating click-shared: %v\n", err)
	}
	return nil, fmt.Errorf("shared sessions are not available in this version of prompt (stay tuned)")
}

func SessionOpenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	activate := resolveBool(pk.Kwargs["activate"], true)
	newName := pk.Kwargs["name"]
	if newName != "" {
		err := validateName(newName, "session")
		if err != nil {
			return nil, err
		}
	}
	update, err := sstore.InsertSessionWithName(ctx, newName, activate)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func makeExternLink(urlStr string) string {
	return fmt.Sprintf(`https://extern?%s`, url.QueryEscape(urlStr))
}

func ScreenWebShareCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("websharing is no longer available")
}

func SessionDeleteCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, 0) // don't force R_Session
	if err != nil {
		return nil, err
	}
	sessionId := ""
	if len(pk.Args) >= 1 {
		ritem, err := resolveSession(ctx, pk.Args[0], ids.SessionId)
		if err != nil {
			return nil, fmt.Errorf("/session:purge error resolving session %q: %w", pk.Args[0], err)
		}
		if ritem == nil {
			return nil, fmt.Errorf("/session:purge session %q not found", pk.Args[0])
		}
		sessionId = ritem.Id
	} else {
		sessionId = ids.SessionId
	}
	if sessionId == "" {
		return nil, fmt.Errorf("/session:purge no sessionid found")
	}
	update, err := sstore.PurgeSession(ctx, sessionId)
	if err != nil {
		return nil, fmt.Errorf("cannot delete session: %v", err)
	}
	return update, nil
}

func SessionArchiveCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, 0) // don't force R_Session
	if err != nil {
		return nil, err
	}
	sessionId := ""
	if len(pk.Args) >= 1 {
		ritem, err := resolveSession(ctx, pk.Args[0], ids.SessionId)
		if err != nil {
			return nil, fmt.Errorf("/session:archive error resolving session %q: %w", pk.Args[0], err)
		}
		if ritem == nil {
			return nil, fmt.Errorf("/session:archive session %q not found", pk.Args[0])
		}
		sessionId = ritem.Id
	} else {
		sessionId = ids.SessionId
	}
	if sessionId == "" {
		return nil, fmt.Errorf("/session:archive no sessionid found")
	}
	archiveVal := true
	if len(pk.Args) >= 2 {
		archiveVal = resolveBool(pk.Args[1], true)
	}
	if archiveVal {
		update, err := sstore.ArchiveSession(ctx, sessionId)
		if err != nil {
			return nil, fmt.Errorf("cannot archive session: %v", err)
		}
		update.Info = &sstore.InfoMsgType{
			InfoMsg: "session archived",
		}
		return update, nil
	} else {
		activate := resolveBool(pk.Kwargs["activate"], false)
		update, err := sstore.UnArchiveSession(ctx, sessionId, activate)
		if err != nil {
			return nil, fmt.Errorf("cannot un-archive session: %v", err)
		}
		update.Info = &sstore.InfoMsgType{
			InfoMsg: "session un-archived",
		}
		return update, nil
	}
}

func SessionShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, err
	}
	session, err := sstore.GetSessionById(ctx, ids.SessionId)
	if err != nil {
		return nil, fmt.Errorf("cannot get session: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("session not found")
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "sessionid", session.SessionId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "name", session.Name))
	if session.SessionIdx != 0 {
		buf.WriteString(fmt.Sprintf("  %-15s %d\n", "index", session.SessionIdx))
	}
	if session.Archived {
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "archived", "true"))
		ts := time.UnixMilli(session.ArchivedTs)
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "archivedts", ts.Format(TsFormatStr)))
	}
	stats, err := sstore.GetSessionStats(ctx, ids.SessionId)
	if err != nil {
		return nil, fmt.Errorf("error getting session stats: %w", err)
	}
	var screenArchiveStr string
	if stats.NumArchivedScreens > 0 {
		screenArchiveStr = fmt.Sprintf(" (%d archived)", stats.NumArchivedScreens)
	}
	buf.WriteString(fmt.Sprintf("  %-15s %d%s\n", "screens", stats.NumScreens, screenArchiveStr))
	buf.WriteString(fmt.Sprintf("  %-15s %d\n", "lines", stats.NumLines))
	buf.WriteString(fmt.Sprintf("  %-15s %d\n", "cmds", stats.NumCmds))
	buf.WriteString(fmt.Sprintf("  %-15s %0.2fM\n", "disksize", float64(stats.DiskStats.TotalSize)/1000000))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "disk-location", stats.DiskStats.Location))
	return &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: "session info",
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}, nil
}

func SessionShowAllCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	sessions, err := sstore.GetBareSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("error retrieving sessions: %v", err)
	}
	var buf bytes.Buffer
	for _, session := range sessions {
		var archivedStr string
		if session.Archived {
			archivedStr = " (archived)"
		}
		sessionIdxStr := "-"
		if session.SessionIdx != 0 {
			sessionIdxStr = strconv.Itoa(int(session.SessionIdx))
		}
		outStr := fmt.Sprintf("%-30s %s  %s\n", session.Name+archivedStr, session.SessionId, sessionIdxStr)
		buf.WriteString(outStr)
	}
	return &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: "all sessions",
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}, nil
}

func SessionSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, err
	}
	var varsUpdated []string
	if pk.Kwargs["name"] != "" {
		newName := pk.Kwargs["name"]
		err = validateName(newName, "session")
		if err != nil {
			return nil, err
		}
		err = sstore.SetSessionName(ctx, ids.SessionId, newName)
		if err != nil {
			return nil, fmt.Errorf("setting session name: %v", err)
		}
		varsUpdated = append(varsUpdated, "name")
	}
	if pk.Kwargs["pos"] != "" {

	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/session:set no updates, can set %s", formatStrs([]string{"name", "pos"}, "or", false))
	}
	bareSession, err := sstore.GetBareSessionById(ctx, ids.SessionId)
	update := &sstore.ModelUpdate{
		Sessions: []*sstore.SessionType{bareSession},
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("session updated %s", formatStrs(varsUpdated, "and", false)),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func SessionCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, 0)
	if err != nil {
		return nil, err
	}
	firstArg := firstArg(pk)
	if firstArg == "" {
		return nil, fmt.Errorf("usage /session [name|id|pos], no param specified")
	}
	ritem, err := resolveSession(ctx, firstArg, ids.SessionId)
	if err != nil {
		return nil, err
	}
	err = sstore.SetActiveSessionId(ctx, ritem.Id)
	if err != nil {
		return nil, err
	}
	update := &sstore.ModelUpdate{
		ActiveSessionId: ritem.Id,
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("switched to session %q", ritem.Name),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func RemoteResetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	initPk, err := ids.Remote.MShell.ReInit(ctx)
	if err != nil {
		return nil, err
	}
	if initPk == nil || initPk.State == nil {
		return nil, fmt.Errorf("invalid initpk received from remote (no remote state)")
	}
	feState := sstore.FeStateFromShellState(initPk.State)
	remoteInst, err := sstore.UpdateRemoteState(ctx, ids.SessionId, ids.ScreenId, ids.Remote.RemotePtr, feState, initPk.State, nil)
	if err != nil {
		return nil, err
	}
	outputStr := "reset remote state"
	cmd, err := makeStaticCmd(ctx, "reset", ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/reset", false, ids, cmd, "", nil)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	update.Sessions = sstore.MakeSessionsUpdateForRemote(ids.SessionId, remoteInst)
	return update, nil
}

func ClearCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if resolveBool(pk.Kwargs["purge"], false) {
		update, err := sstore.PurgeScreenLines(ctx, ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("clearing screen: %v", err)
		}
		update.Info = &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("screen cleared (all lines purged)"),
			TimeoutMs: 2000,
		}
		return update, nil
	} else {
		update, err := sstore.ArchiveScreenLines(ctx, ids.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("clearing screen: %v", err)
		}
		update.Info = &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("screen cleared"),
			TimeoutMs: 2000,
		}
		return update, nil
	}

}

func HistoryPurgeCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/history:purge requires at least one argument (history id)")
	}
	var historyIds []string
	for _, historyArg := range pk.Args {
		_, err := uuid.Parse(historyArg)
		if err != nil {
			return nil, fmt.Errorf("invalid historyid (must be uuid)")
		}
		historyIds = append(historyIds, historyArg)
	}
	historyItemsRemoved, err := sstore.PurgeHistoryByIds(ctx, historyIds)
	if err != nil {
		return nil, fmt.Errorf("/history:purge error purging items: %v", err)
	}
	update := &sstore.ModelUpdate{}
	for _, historyItem := range historyItemsRemoved {
		if historyItem.LineId == "" {
			continue
		}
		lineObj := &sstore.LineType{
			ScreenId: historyItem.ScreenId,
			LineId:   historyItem.LineId,
			Remove:   true,
		}
		update.Lines = append(update.Lines, lineObj)
	}
	return update, nil
}

const HistoryViewPageSize = 50

var cmdFilterLs = regexp.MustCompile(`^ls(\s|$)`)
var cmdFilterCd = regexp.MustCompile(`^cd(\s|$)`)

func historyCmdFilter(hitem *sstore.HistoryItemType) bool {
	cmdStr := hitem.CmdStr
	if cmdStr == "" || strings.Index(cmdStr, ";") != -1 || strings.Index(cmdStr, "\n") != -1 {
		return true
	}
	if cmdFilterLs.MatchString(cmdStr) {
		return false
	}
	if cmdFilterCd.MatchString(cmdStr) {
		return false
	}
	return true
}

func HistoryViewAllCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	_, err := resolveUiIds(ctx, pk, 0)
	if err != nil {
		return nil, err
	}
	offset, err := resolveNonNegInt(pk.Kwargs["offset"], 0)
	if err != nil {
		return nil, err
	}
	rawOffset, err := resolveNonNegInt(pk.Kwargs["rawoffset"], 0)
	if err != nil {
		return nil, err
	}
	opts := sstore.HistoryQueryOpts{MaxItems: HistoryViewPageSize, Offset: offset, RawOffset: rawOffset}
	if pk.Kwargs["text"] != "" {
		opts.SearchText = pk.Kwargs["text"]
	}
	if pk.Kwargs["searchsession"] != "" {
		sessionId, err := resolveSessionArg(pk.Kwargs["searchsession"])
		if err != nil {
			return nil, fmt.Errorf("invalid searchsession: %v", err)
		}
		opts.SessionId = sessionId
	}
	if pk.Kwargs["searchremote"] != "" {
		rptr, err := resolveRemoteArg(pk.Kwargs["searchremote"])
		if err != nil {
			return nil, fmt.Errorf("invalid searchremote: %v", err)
		}
		if rptr != nil {
			opts.RemoteId = rptr.RemoteId
		}
	}
	if pk.Kwargs["fromts"] != "" {
		fromTs, err := resolvePosInt(pk.Kwargs["fromts"], 0)
		if err != nil {
			return nil, fmt.Errorf("invalid fromts (must be unixtime (milliseconds): %v", err)
		}
		if fromTs > 0 {
			opts.FromTs = int64(fromTs)
		}
	}
	if pk.Kwargs["meta"] != "" {
		opts.NoMeta = !resolveBool(pk.Kwargs["meta"], true)
	}
	if resolveBool(pk.Kwargs["filter"], false) {
		opts.FilterFn = historyCmdFilter
	}
	if err != nil {
		return nil, fmt.Errorf("invalid meta arg (must be boolean): %v", err)
	}
	hresult, err := sstore.GetHistoryItems(ctx, opts)
	if err != nil {
		return nil, err
	}
	hvdata := &sstore.HistoryViewData{
		Items:         hresult.Items,
		Offset:        hresult.Offset,
		RawOffset:     hresult.RawOffset,
		NextRawOffset: hresult.NextRawOffset,
		HasMore:       hresult.HasMore,
	}
	lines, cmds, err := sstore.GetLineCmdsFromHistoryItems(ctx, hvdata.Items)
	if err != nil {
		return nil, err
	}
	hvdata.Lines = lines
	hvdata.Cmds = cmds
	update := &sstore.ModelUpdate{
		HistoryViewData: hvdata,
		MainView:        sstore.MainViewHistory,
	}
	return update, nil
}

const DefaultMaxHistoryItems = 10000

func HistoryCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Remote)
	if err != nil {
		return nil, err
	}
	maxItems, err := resolvePosInt(pk.Kwargs["maxitems"], DefaultMaxHistoryItems)
	if err != nil {
		return nil, fmt.Errorf("invalid maxitems value '%s' (must be a number): %v", pk.Kwargs["maxitems"], err)
	}
	if maxItems < 0 {
		return nil, fmt.Errorf("invalid maxitems value '%d' (cannot be negative)", maxItems)
	}
	if maxItems == 0 {
		maxItems = DefaultMaxHistoryItems
	}
	htype := HistoryTypeScreen
	hSessionId := ids.SessionId
	hScreenId := ids.ScreenId
	if pk.Kwargs["type"] != "" {
		htype = pk.Kwargs["type"]
		if htype != HistoryTypeScreen && htype != HistoryTypeSession && htype != HistoryTypeGlobal {
			return nil, fmt.Errorf("invalid history type '%s', valid types: %s", htype, formatStrs([]string{HistoryTypeScreen, HistoryTypeSession, HistoryTypeGlobal}, "or", false))
		}
	}
	if htype == HistoryTypeGlobal {
		hSessionId = ""
		hScreenId = ""
	} else if htype == HistoryTypeSession {
		hScreenId = ""
	}
	hopts := sstore.HistoryQueryOpts{MaxItems: maxItems, SessionId: hSessionId, ScreenId: hScreenId}
	hresult, err := sstore.GetHistoryItems(ctx, hopts)
	if err != nil {
		return nil, err
	}
	show := !resolveBool(pk.Kwargs["noshow"], false)
	if show {
		err = sstore.UpdateCurrentActivity(ctx, sstore.ActivityUpdate{HistoryView: 1})
		if err != nil {
			log.Printf("error updating current activity (history): %v\n", err)
		}
	}
	update := &sstore.ModelUpdate{}
	update.History = &sstore.HistoryInfoType{
		HistoryType: htype,
		SessionId:   ids.SessionId,
		ScreenId:    ids.ScreenId,
		Items:       hresult.Items,
		Show:        show,
	}
	return update, nil
}

func splitLinesForInfo(str string) []string {
	rtn := strings.Split(str, "\n")
	if rtn[len(rtn)-1] == "" {
		return rtn[:len(rtn)-1]
	}
	return rtn
}

func resizeRunningCommand(ctx context.Context, cmd *sstore.CmdType, newCols int) error {
	siPk := packet.MakeSpecialInputPacket()
	siPk.CK = base.MakeCommandKey(cmd.ScreenId, cmd.LineId)
	siPk.WinSize = &packet.WinSize{Rows: int(cmd.TermOpts.Rows), Cols: newCols}
	msh := remote.GetRemoteById(cmd.Remote.RemoteId)
	if msh == nil {
		return fmt.Errorf("cannot resize, cmd remote not found")
	}
	err := msh.SendSpecialInput(siPk)
	if err != nil {
		return err
	}
	newTermOpts := cmd.TermOpts
	newTermOpts.Cols = int64(newCols)
	err = sstore.UpdateCmdTermOpts(ctx, cmd.ScreenId, cmd.LineId, newTermOpts)
	if err != nil {
		return err
	}
	return nil
}

func ScreenResizeCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	colsStr := pk.Kwargs["cols"]
	if colsStr == "" {
		return nil, fmt.Errorf("/screen:resize requires a numeric 'cols' argument")
	}
	cols, err := strconv.Atoi(colsStr)
	if err != nil {
		return nil, fmt.Errorf("/screen:resize requires a numeric 'cols' argument: %v", err)
	}
	if cols <= 0 {
		return nil, fmt.Errorf("/screen:resize invalid zero/negative 'cols' argument")
	}
	cols = base.BoundInt(cols, shexec.MinTermCols, shexec.MaxTermCols)
	runningCmds, err := sstore.GetRunningScreenCmds(ctx, ids.ScreenId)
	if err != nil {
		return nil, fmt.Errorf("/screen:resize cannot get running commands: %v", err)
	}
	if len(runningCmds) == 0 {
		return nil, nil
	}
	for _, cmd := range runningCmds {
		if int(cmd.TermOpts.Cols) != cols {
			resizeRunningCommand(ctx, cmd, cols)
		}
	}
	return nil, nil
}

func LineCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/line requires a subcommand: %s", formatStrs([]string{"show", "star", "hide", "purge", "setheight", "set"}, "or", false))
}

func LineSetHeightCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) != 2 {
		return nil, fmt.Errorf("/line:setheight requires 2 arguments (linearg and height)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	heightVal, err := resolveNonNegInt(pk.Args[1], 0)
	if err != nil {
		return nil, fmt.Errorf("/line:setheight invalid height val: %v", err)
	}
	if heightVal > 10000 {
		return nil, fmt.Errorf("/line:setheight invalid height val (too large): %d", heightVal)
	}
	err = sstore.UpdateLineHeight(ctx, ids.ScreenId, lineId, heightVal)
	if err != nil {
		return nil, fmt.Errorf("/line:setheight error updating height: %v", err)
	}
	// we don't need to pass the updated line height (it is "write only")
	return nil, nil
}

func LineSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) != 1 {
		return nil, fmt.Errorf("/line:set requires 1 argument (linearg)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	var varsUpdated []string
	if renderer, found := pk.Kwargs[KwArgRenderer]; found {
		if err = validateRenderer(renderer); err != nil {
			return nil, fmt.Errorf("invalid renderer value: %w", err)
		}
		err = sstore.UpdateLineRenderer(ctx, ids.ScreenId, lineId, renderer)
		if err != nil {
			return nil, fmt.Errorf("error changing line renderer: %v", err)
		}
		varsUpdated = append(varsUpdated, KwArgRenderer)
	}
	if view, found := pk.Kwargs[KwArgView]; found {
		if err = validateRenderer(view); err != nil {
			return nil, fmt.Errorf("invalid view value: %w", err)
		}
		err = sstore.UpdateLineRenderer(ctx, ids.ScreenId, lineId, view)
		if err != nil {
			return nil, fmt.Errorf("error changing line view: %v", err)
		}
		varsUpdated = append(varsUpdated, KwArgView)
	}
	if stateJson, found := pk.Kwargs[KwArgState]; found {
		if len(stateJson) > sstore.MaxLineStateSize {
			return nil, fmt.Errorf("invalid state value (too large), size[%d], max[%d]", len(stateJson), sstore.MaxLineStateSize)
		}
		var stateMap map[string]any
		err = json.Unmarshal([]byte(stateJson), &stateMap)
		if err != nil {
			return nil, fmt.Errorf("invalid state value, cannot parse json: %v", err)
		}
		err = sstore.UpdateLineState(ctx, ids.ScreenId, lineId, stateMap)
		if err != nil {
			return nil, fmt.Errorf("cannot update linestate: %v", err)
		}
		varsUpdated = append(varsUpdated, KwArgState)
	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/line:set requires a value to set: %s", formatStrs([]string{KwArgView, KwArgState}, "or", false))
	}
	updatedLine, err := sstore.GetLineById(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("/line:set cannot retrieve updated line: %v", err)
	}
	update := &sstore.ModelUpdate{
		Line: updatedLine,
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("line updated %s", formatStrs(varsUpdated, "and", false)),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func LineViewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) != 3 {
		return nil, fmt.Errorf("usage /line:view [session] [screen] [line]")
	}
	sessionArg := pk.Args[0]
	screenArg := pk.Args[1]
	lineArg := pk.Args[2]
	sessionId, err := resolveSessionArg(sessionArg)
	if err != nil {
		return nil, fmt.Errorf("/line:view invalid session arg: %v", err)
	}
	if sessionId == "" {
		return nil, fmt.Errorf("/line:view no session found")
	}
	screenRItem, err := resolveSessionScreen(ctx, sessionId, screenArg, "")
	if err != nil {
		return nil, fmt.Errorf("/line:view invalid screen arg: %v", err)
	}
	if screenRItem == nil {
		return nil, fmt.Errorf("/line:view no screen found")
	}
	screen, err := sstore.GetScreenById(ctx, screenRItem.Id)
	if err != nil {
		return nil, fmt.Errorf("/line:view could not get screen: %v", err)
	}
	lineRItem, err := resolveLine(ctx, sessionId, screen.ScreenId, lineArg, "")
	if err != nil {
		return nil, fmt.Errorf("/line:view invalid line arg: %v", err)
	}
	update, err := sstore.SwitchScreenById(ctx, sessionId, screenRItem.Id)
	if err != nil {
		return nil, err
	}
	if lineRItem != nil {
		updateMap := make(map[string]interface{})
		updateMap[sstore.ScreenField_SelectedLine] = lineRItem.Num
		updateMap[sstore.ScreenField_AnchorLine] = lineRItem.Num
		updateMap[sstore.ScreenField_AnchorOffset] = 0
		screen, err = sstore.UpdateScreen(ctx, screenRItem.Id, updateMap)
		if err != nil {
			return nil, err
		}
		update.Screens = []*sstore.ScreenType{screen}
	}
	return update, nil
}

func BookmarksShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	// no resolve ui ids!
	var tagName string // defaults to ''
	if len(pk.Args) > 0 {
		tagName = pk.Args[0]
	}
	bms, err := sstore.GetBookmarks(ctx, tagName)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve bookmarks: %v", err)
	}
	err = sstore.UpdateCurrentActivity(ctx, sstore.ActivityUpdate{BookmarksView: 1})
	if err != nil {
		log.Printf("error updating current activity (bookmarks): %v\n", err)
	}
	update := &sstore.ModelUpdate{
		MainView:  sstore.MainViewBookmarks,
		Bookmarks: bms,
	}
	return update, nil
}

func BookmarkSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/bookmark:set requires one argument (bookmark id)")
	}
	bookmarkArg := pk.Args[0]
	bookmarkId, err := sstore.GetBookmarkIdByArg(ctx, bookmarkArg)
	if err != nil {
		return nil, fmt.Errorf("error trying to resolve bookmark: %v", err)
	}
	if bookmarkId == "" {
		return nil, fmt.Errorf("bookmark not found")
	}
	editMap := make(map[string]interface{})
	if descStr, found := pk.Kwargs["desc"]; found {
		editMap[sstore.BookmarkField_Desc] = descStr
	}
	if cmdStr, found := pk.Kwargs["cmdstr"]; found {
		editMap[sstore.BookmarkField_CmdStr] = cmdStr
	}
	if len(editMap) == 0 {
		return nil, fmt.Errorf("no fields set, can set %s", formatStrs([]string{"desc", "cmdstr"}, "or", false))
	}
	err = sstore.EditBookmark(ctx, bookmarkId, editMap)
	if err != nil {
		return nil, fmt.Errorf("error trying to edit bookmark: %v", err)
	}
	bm, err := sstore.GetBookmarkById(ctx, bookmarkId, "")
	if err != nil {
		return nil, fmt.Errorf("error retrieving edited bookmark: %v", err)
	}
	return &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg: "bookmark edited",
		},
		Bookmarks: []*sstore.BookmarkType{bm},
	}, nil
}

func BookmarkDeleteCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/bookmark:delete requires one argument (bookmark id)")
	}
	bookmarkArg := pk.Args[0]
	bookmarkId, err := sstore.GetBookmarkIdByArg(ctx, bookmarkArg)
	if err != nil {
		return nil, fmt.Errorf("error trying to resolve bookmark: %v", err)
	}
	if bookmarkId == "" {
		return nil, fmt.Errorf("bookmark not found")
	}
	err = sstore.DeleteBookmark(ctx, bookmarkId)
	if err != nil {
		return nil, fmt.Errorf("error deleting bookmark: %v", err)
	}
	bm := &sstore.BookmarkType{BookmarkId: bookmarkId, Remove: true}
	return &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg: "bookmark deleted",
		},
		Bookmarks: []*sstore.BookmarkType{bm},
	}, nil
}

func LineBookmarkCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:bookmark requires an argument (line number or id)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	if lineId == "" {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	_, cmdObj, err := sstore.GetLineCmdByLineId(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("/line:bookmark error getting line: %v", err)
	}
	if cmdObj == nil {
		return nil, fmt.Errorf("cannot bookmark non-cmd line")
	}
	existingBmIds, err := sstore.GetBookmarkIdsByCmdStr(ctx, cmdObj.CmdStr)
	if err != nil {
		return nil, fmt.Errorf("error trying to retrieve current boookmarks: %v", err)
	}
	var newBmId string
	if len(existingBmIds) > 0 {
		newBmId = existingBmIds[0]
	} else {
		newBm := &sstore.BookmarkType{
			BookmarkId:  uuid.New().String(),
			CreatedTs:   time.Now().UnixMilli(),
			CmdStr:      cmdObj.CmdStr,
			Alias:       "",
			Tags:        nil,
			Description: "",
		}
		err = sstore.InsertBookmark(ctx, newBm)
		if err != nil {
			return nil, fmt.Errorf("cannot insert bookmark: %v", err)
		}
		newBmId = newBm.BookmarkId
	}
	bms, err := sstore.GetBookmarks(ctx, "")
	update := &sstore.ModelUpdate{
		MainView:         sstore.MainViewBookmarks,
		Bookmarks:        bms,
		SelectedBookmark: newBmId,
	}
	return update, nil
}

func LinePinCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, nil
}

func LineStarCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:star requires an argument (line number or id)")
	}
	if len(pk.Args) > 2 {
		return nil, fmt.Errorf("/line:star only takes up to 2 arguments (line-number and star-value)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	if lineId == "" {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	starVal, err := resolveNonNegInt(pk.Args[1], 1)
	if err != nil {
		return nil, fmt.Errorf("/line:star invalid star-value (not integer): %v", err)
	}
	if starVal > 5 {
		return nil, fmt.Errorf("/line:star invalid star-value must be in the range of 0-5")
	}
	err = sstore.UpdateLineStar(ctx, ids.ScreenId, lineId, starVal)
	if err != nil {
		return nil, fmt.Errorf("/line:star error updating star value: %v", err)
	}
	lineObj, err := sstore.GetLineById(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("/line:star error getting line: %v", err)
	}
	if lineObj == nil {
		// no line (which is strange given we checked for it above).  just return a nop.
		return nil, nil
	}
	return &sstore.ModelUpdate{Line: lineObj}, nil
}

func LineArchiveCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:archive requires an argument (line number or id)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	if lineId == "" {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	shouldArchive := true
	if len(pk.Args) >= 2 {
		shouldArchive = resolveBool(pk.Args[1], true)
	}
	err = sstore.SetLineArchivedById(ctx, ids.ScreenId, lineId, shouldArchive)
	if err != nil {
		return nil, fmt.Errorf("/line:archive error updating hidden status: %v", err)
	}
	lineObj, err := sstore.GetLineById(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("/line:archive error getting line: %v", err)
	}
	if lineObj == nil {
		// no line (which is strange given we checked for it above).  just return a nop.
		return nil, nil
	}
	return &sstore.ModelUpdate{Line: lineObj}, nil
}

func LinePurgeCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:purge requires at least one argument (line number or id)")
	}
	var lineIds []string
	for _, lineArg := range pk.Args {
		lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
		if err != nil {
			return nil, fmt.Errorf("error looking up lineid: %v", err)
		}
		if lineId == "" {
			return nil, fmt.Errorf("line %q not found", lineArg)
		}
		lineIds = append(lineIds, lineId)
	}
	err = sstore.PurgeLinesByIds(ctx, ids.ScreenId, lineIds)
	if err != nil {
		return nil, fmt.Errorf("/line:purge error purging lines: %v", err)
	}
	update := &sstore.ModelUpdate{}
	for _, lineId := range lineIds {
		lineObj := &sstore.LineType{
			ScreenId: ids.ScreenId,
			LineId:   lineId,
			Remove:   true,
		}
		update.Lines = append(update.Lines, lineObj)
	}
	return update, nil
}

func LineShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:show requires an argument (line number or id)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	if lineId == "" {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	line, cmd, err := sstore.GetLineCmdByLineId(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("error getting line: %v", err)
	}
	if line == nil {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "screenid", line.ScreenId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "lineid", line.LineId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "type", line.LineType))
	lineNumStr := strconv.FormatInt(line.LineNum, 10)
	if line.LineNumTemp {
		lineNumStr = "~" + lineNumStr
	}
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "linenum", lineNumStr))
	ts := time.UnixMilli(line.Ts)
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "ts", ts.Format(TsFormatStr)))
	if line.Ephemeral {
		buf.WriteString(fmt.Sprintf("  %-15s %v\n", "ephemeral", true))
	}
	if line.Renderer != "" {
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "renderer", line.Renderer))
	} else {
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "renderer", "terminal"))
	}
	if cmd != nil {
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "remote", cmd.Remote.MakeFullRemoteRef()))
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "status", cmd.Status))
		if cmd.FeState["cwd"] != "" {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "cwd", cmd.FeState["cwd"]))
		}
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "termopts", formatTermOpts(cmd.TermOpts)))
		if cmd.TermOpts != cmd.OrigTermOpts {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "orig-termopts", formatTermOpts(cmd.OrigTermOpts)))
		}
		if cmd.RtnState {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "rtnstate", "true"))
		}
		stat, _ := sstore.StatCmdPtyFile(ctx, cmd.ScreenId, cmd.LineId)
		if stat == nil {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "file", "-"))
		} else {
			fileDataStr := fmt.Sprintf("v%d data=%d offset=%d max=%s", stat.Version, stat.DataSize, stat.FileOffset, scbase.NumFormatB2(stat.MaxSize))
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "file", stat.Location))
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "file-data", fileDataStr))
		}
		if cmd.DoneTs != 0 {
			doneTs := time.UnixMilli(cmd.DoneTs)
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "donets", doneTs.Format(TsFormatStr)))
			buf.WriteString(fmt.Sprintf("  %-15s %d\n", "exitcode", cmd.ExitCode))
			buf.WriteString(fmt.Sprintf("  %-15s %dms\n", "duration", cmd.DurationMs))
		}
	}
	stateStr := dbutil.QuickJson(line.LineState)
	if len(stateStr) > 80 {
		stateStr = stateStr[0:77] + "..."
	}
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "state", stateStr))
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("line %d info", line.LineNum),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func SetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	var setMap map[string]map[string]string
	setMap = make(map[string]map[string]string)
	_, err := resolveUiIds(ctx, pk, 0) // best effort
	if err != nil {
		return nil, err
	}
	for argIdx, rawArgVal := range pk.Args {
		eqIdx := strings.Index(rawArgVal, "=")
		if eqIdx == -1 {
			return nil, fmt.Errorf("/set invalid argument %d, does not contain an '='", argIdx)
		}
		argName := rawArgVal[:eqIdx]
		argVal := rawArgVal[eqIdx+1:]
		ok, scopeName, varName := resolveSetArg(argName)
		if !ok {
			return nil, fmt.Errorf("/set invalid setvar %q", argName)
		}
		if _, ok := setMap[scopeName]; !ok {
			setMap[scopeName] = make(map[string]string)
		}
		setMap[scopeName][varName] = argVal
	}
	return nil, nil
}

func makeStreamFilePk(ids resolvedIds, pk *scpacket.FeCommandPacketType) (*packet.StreamFilePacketType, error) {
	cwd := ids.Remote.FeState["cwd"]
	fileArg := pk.Args[0]
	if fileArg == "" {
		return nil, fmt.Errorf("/view:stat file argument must be set (cannot be empty)")
	}
	streamPk := packet.MakeStreamFilePacket()
	streamPk.ReqId = uuid.New().String()
	if filepath.IsAbs(fileArg) {
		streamPk.Path = fileArg
	} else {
		streamPk.Path = filepath.Join(cwd, fileArg)
	}
	return streamPk, nil
}

func ViewStatCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/view:stat requires an argument (file name)")
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	streamPk, err := makeStreamFilePk(ids, pk)
	if err != nil {
		return nil, err
	}
	streamPk.StatOnly = true
	msh := ids.Remote.MShell
	iter, err := msh.StreamFile(ctx, streamPk)
	if err != nil {
		return nil, fmt.Errorf("/view:stat error: %v", err)
	}
	defer iter.Close()
	respIf, err := iter.Next(ctx)
	if err != nil {
		return nil, fmt.Errorf("/view:stat error getting response: %v", err)
	}
	resp, ok := respIf.(*packet.StreamFileResponseType)
	if !ok {
		return nil, fmt.Errorf("/view:stat error, bad response packet type: %T", respIf)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("/view:stat error: %s", resp.Error)
	}
	if resp.Info == nil {
		return nil, fmt.Errorf("/view:stat error, no file info")
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "path", resp.Info.Name))
	buf.WriteString(fmt.Sprintf("  %-15s %d\n", "size", resp.Info.Size))
	modTs := time.UnixMilli(resp.Info.ModTs)
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "modts", modTs.Format(TsFormatStr)))
	buf.WriteString(fmt.Sprintf("  %-15s %v\n", "isdir", resp.Info.IsDir))
	modeStr := fs.FileMode(resp.Info.Perm).String()
	if len(modeStr) > 9 {
		modeStr = modeStr[len(modeStr)-9:]
	}
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "perms", modeStr))
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("view stat %q", streamPk.Path),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func ViewTestCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/view:test requires an argument (file name)")
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	streamPk, err := makeStreamFilePk(ids, pk)
	if err != nil {
		return nil, err
	}
	msh := ids.Remote.MShell
	iter, err := msh.StreamFile(ctx, streamPk)
	if err != nil {
		return nil, fmt.Errorf("/view:test error: %v", err)
	}
	defer iter.Close()
	respIf, err := iter.Next(ctx)
	if err != nil {
		return nil, fmt.Errorf("/view:test error getting response: %v", err)
	}
	resp, ok := respIf.(*packet.StreamFileResponseType)
	if !ok {
		return nil, fmt.Errorf("/view:test error, bad response packet type: %T", respIf)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("/view:test error: %s", resp.Error)
	}
	if resp.Info == nil {
		return nil, fmt.Errorf("/view:test error, no file info")
	}
	var buf bytes.Buffer
	var numPackets int
	for {
		dataPkIf, err := iter.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("/view:test error while getting data: %w", err)
		}
		if dataPkIf == nil {
			break
		}
		dataPk, ok := dataPkIf.(*packet.FileDataPacketType)
		if !ok {
			return nil, fmt.Errorf("/view:test invalid data packet type: %T", dataPkIf)
		}
		if dataPk.Error != "" {
			return nil, fmt.Errorf("/view:test error returned while getting data: %s", dataPk.Error)
		}
		numPackets++
		buf.Write(dataPk.Data)
	}
	buf.WriteString(fmt.Sprintf("\n\ntotal packets: %d\n", numPackets))
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("view file %q", streamPk.Path),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func CodeEditCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("%s requires an argument (file name)", GetCmdStr(pk))
	}
	// TODO more error checking on filename format?
	if pk.Args[0] == "" {
		return nil, fmt.Errorf("%s argument cannot be empty", GetCmdStr(pk))
	}
	langArg, err := getLangArg(pk)
	if err != nil {
		return nil, fmt.Errorf("%s invalid 'lang': %v", GetCmdStr(pk), err)
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	outputStr := fmt.Sprintf("%s %q", GetCmdStr(pk), pk.Args[0])
	cmd, err := makeStaticCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	// set the line state
	lineState := make(map[string]any)
	lineState[sstore.LineState_Source] = "file"
	lineState[sstore.LineState_File] = pk.Args[0]
	if GetCmdStr(pk) == "codeview" {
		lineState[sstore.LineState_Mode] = "view"
	} else {
		lineState[sstore.LineState_Mode] = "edit"
	}
	if langArg != "" {
		lineState[sstore.LineState_Lang] = langArg
	}
	update, err := addLineForCmd(ctx, "/"+GetCmdStr(pk), true, ids, cmd, "code", lineState)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	return update, nil
}

func CSVViewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("%s requires an argument (file name)", GetCmdStr(pk))
	}
	// TODO more error checking on filename format?
	if pk.Args[0] == "" {
		return nil, fmt.Errorf("%s argument cannot be empty", GetCmdStr(pk))
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	outputStr := fmt.Sprintf("%s %q", GetCmdStr(pk), pk.Args[0])
	cmd, err := makeStaticCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	// set the line state
	lineState := make(map[string]any)
	lineState[sstore.LineState_Source] = "file"
	lineState[sstore.LineState_File] = pk.Args[0]
	update, err := addLineForCmd(ctx, "/"+GetCmdStr(pk), true, ids, cmd, "csv", lineState)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	return update, nil
}

func ImageViewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("%s requires an argument (file name)", GetCmdStr(pk))
	}
	// TODO more error checking on filename format?
	if pk.Args[0] == "" {
		return nil, fmt.Errorf("%s argument cannot be empty", GetCmdStr(pk))
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	outputStr := fmt.Sprintf("%s %q", GetCmdStr(pk), pk.Args[0])
	cmd, err := makeStaticCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	// set the line state
	lineState := make(map[string]any)
	lineState[sstore.LineState_Source] = "file"
	lineState[sstore.LineState_File] = pk.Args[0]
	update, err := addLineForCmd(ctx, "/"+GetCmdStr(pk), false, ids, cmd, "image", lineState)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	return update, nil
}

func MarkdownViewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("%s requires an argument (file name)", GetCmdStr(pk))
	}
	// TODO more error checking on filename format?
	if pk.Args[0] == "" {
		return nil, fmt.Errorf("%s argument cannot be empty", GetCmdStr(pk))
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	outputStr := fmt.Sprintf("%s %q", GetCmdStr(pk), pk.Args[0])
	cmd, err := makeStaticCmd(ctx, GetCmdStr(pk), ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	// set the line state
	lineState := make(map[string]any)
	lineState[sstore.LineState_Source] = "file"
	lineState[sstore.LineState_File] = pk.Args[0]
	update, err := addLineForCmd(ctx, "/"+GetCmdStr(pk), false, ids, cmd, "markdown", lineState)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	return update, nil
}

func EditTestCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/edit:test requires an argument (file name)")
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_RemoteConnected)
	if err != nil {
		return nil, err
	}
	content, ok := pk.Kwargs["content"]
	if !ok {
		return nil, fmt.Errorf("/edit:test no content for file specified")
	}
	fileArg := pk.Args[0]
	if fileArg == "" {
		return nil, fmt.Errorf("/view:stat file argument must be set (cannot be empty)")
	}
	writePk := packet.MakeWriteFilePacket()
	writePk.ReqId = uuid.New().String()
	writePk.UseTemp = true
	cwd := ids.Remote.FeState["cwd"]
	if filepath.IsAbs(fileArg) {
		writePk.Path = fileArg
	} else {
		writePk.Path = filepath.Join(cwd, fileArg)
	}
	msh := ids.Remote.MShell
	iter, err := msh.PacketRpcIter(ctx, writePk)
	if err != nil {
		return nil, fmt.Errorf("/edit:test error: %v", err)
	}
	// first packet should be WriteFileReady
	readyIf, err := iter.Next(ctx)
	if err != nil {
		return nil, fmt.Errorf("/edit:test error while getting ready response: %w", err)
	}
	readyPk, ok := readyIf.(*packet.WriteFileReadyPacketType)
	if !ok {
		return nil, fmt.Errorf("/edit:test bad ready packet received: %T", readyIf)
	}
	if readyPk.Error != "" {
		return nil, fmt.Errorf("/edit:test %s", readyPk.Error)
	}
	dataPk := packet.MakeFileDataPacket(writePk.ReqId)
	dataPk.Data = []byte(content)
	dataPk.Eof = true
	err = msh.SendFileData(dataPk)
	if err != nil {
		return nil, fmt.Errorf("/edit:test error sending data packet: %v", err)
	}
	doneIf, err := iter.Next(ctx)
	if err != nil {
		return nil, fmt.Errorf("/edit:test error while getting done response: %w", err)
	}
	donePk, ok := doneIf.(*packet.WriteFileDonePacketType)
	if !ok {
		return nil, fmt.Errorf("/edit:test bad done packet received: %T", doneIf)
	}
	if donePk.Error != "" {
		return nil, fmt.Errorf("/edit:test %s", donePk.Error)
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("edit test, wrote %q", writePk.Path),
		},
	}
	return update, nil
}

func SignalCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/signal requires a first argument (line number or id)")
	}
	if len(pk.Args) == 1 {
		return nil, fmt.Errorf("/signal requires a second argument (signal name)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.ScreenId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	line, cmd, err := sstore.GetLineCmdByLineId(ctx, ids.ScreenId, lineId)
	if err != nil {
		return nil, fmt.Errorf("error getting line: %v", err)
	}
	if line == nil {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	if cmd == nil {
		return nil, fmt.Errorf("line %q does not have a command", lineArg)
	}
	if cmd.Status != sstore.CmdStatusRunning {
		return nil, fmt.Errorf("line %q command is not running, cannot send signal", lineArg)
	}
	sigArg := pk.Args[1]
	if isAllDigits(sigArg) {
		val, _ := strconv.Atoi(sigArg)
		if val <= 0 || val > MaxSignalNum {
			return nil, fmt.Errorf("signal number is out of bounds: %q", sigArg)
		}
	} else if !strings.HasPrefix(sigArg, "SIG") {
		sigArg = "SIG" + sigArg
	}
	sigArg = strings.ToUpper(sigArg)
	if len(sigArg) > 12 {
		return nil, fmt.Errorf("invalid signal (too long): %q", sigArg)
	}
	if !sigNameRe.MatchString(sigArg) {
		return nil, fmt.Errorf("invalid signal name/number: %q", sigArg)
	}
	msh := remote.GetRemoteById(cmd.Remote.RemoteId)
	if msh == nil {
		return nil, fmt.Errorf("cannot send signal, no remote found for command")
	}
	if !msh.IsConnected() {
		return nil, fmt.Errorf("cannot send signal, remote is not connected")
	}
	siPk := packet.MakeSpecialInputPacket()
	siPk.CK = base.MakeCommandKey(cmd.ScreenId, cmd.LineId)
	siPk.SigName = sigArg
	err = msh.SendSpecialInput(siPk)
	if err != nil {
		return nil, fmt.Errorf("cannot send signal: %v", err)
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg: fmt.Sprintf("sent line %s signal %s", lineArg, sigArg),
		},
	}
	return update, nil
}

func KillServerCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	go func() {
		log.Printf("received /killserver, shutting down\n")
		time.Sleep(1 * time.Second)
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	}()
	return nil, nil
}

func ClientCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/client requires a subcommand: %s", formatStrs([]string{"show", "set"}, "or", false))
}

func ClientNotifyUpdateWriterCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	pcloud.ResetUpdateWriterNumFailures()
	sstore.NotifyUpdateWriter()
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg: fmt.Sprintf("notified update writer"),
		},
	}
	return update, nil
}

func boolToStr(v bool, trueStr string, falseStr string) string {
	if v {
		return trueStr
	}
	return falseStr
}

func ClientAcceptTosCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	clientOpts := clientData.ClientOpts
	clientOpts.AcceptedTos = time.Now().UnixMilli()
	err = sstore.SetClientOpts(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("error updating client data: %v", err)
	}
	clientData, err = sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve updated client data: %v", err)
	}
	update := &sstore.ModelUpdate{
		ClientData: clientData,
	}
	return update, nil
}

func validateOpenAIAPIToken(key string) error {
	if len(key) == 0 {
		return fmt.Errorf("invalid openai token, zero length")
	}
	if len(key) > MaxOpenAIAPITokenLen {
		return fmt.Errorf("invalid openai token, too long")
	}
	for idx, ch := range key {
		if !unicode.IsPrint(ch) {
			return fmt.Errorf("invalid openai token, char at idx:%d is invalid %q", idx, string(ch))
		}
	}
	return nil
}

func validateOpenAIModel(model string) error {
	if len(model) == 0 {
		return nil
	}
	if len(model) > MaxOpenAIModelLen {
		return fmt.Errorf("invalid openai model, too long")
	}
	for idx, ch := range model {
		if !unicode.IsPrint(ch) {
			return fmt.Errorf("invalid openai model, char at idx:%d is invalid %q", idx, string(ch))
		}
	}
	return nil
}

func ClientSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	var varsUpdated []string
	if fontSizeStr, found := pk.Kwargs["termfontsize"]; found {
		newFontSize, err := resolveNonNegInt(fontSizeStr, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid termfontsize, must be a number between 8-15: %v", err)
		}
		if newFontSize < TermFontSizeMin || newFontSize > TermFontSizeMax {
			return nil, fmt.Errorf("invalid termfontsize, must be a number between %d-%d", TermFontSizeMin, TermFontSizeMax)
		}
		feOpts := clientData.FeOpts
		feOpts.TermFontSize = newFontSize
		err = sstore.UpdateClientFeOpts(ctx, feOpts)
		if err != nil {
			return nil, fmt.Errorf("error updating client feopts: %v", err)
		}
		varsUpdated = append(varsUpdated, "termfontsize")
	}
	if apiToken, found := pk.Kwargs["openaiapitoken"]; found {
		err = validateOpenAIAPIToken(apiToken)
		if err != nil {
			return nil, err
		}
		varsUpdated = append(varsUpdated, "openaiapitoken")
		aiOpts := clientData.OpenAIOpts
		if aiOpts == nil {
			aiOpts = &sstore.OpenAIOptsType{}
			clientData.OpenAIOpts = aiOpts
		}
		aiOpts.APIToken = apiToken
		err = sstore.UpdateClientOpenAIOpts(ctx, *aiOpts)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai api token: %v", err)
		}
	}
	if aiModel, found := pk.Kwargs["openaimodel"]; found {
		err = validateOpenAIModel(aiModel)
		if err != nil {
			return nil, err
		}
		varsUpdated = append(varsUpdated, "openaimodel")
		aiOpts := clientData.OpenAIOpts
		if aiOpts == nil {
			aiOpts = &sstore.OpenAIOptsType{}
			clientData.OpenAIOpts = aiOpts
		}
		aiOpts.Model = aiModel
		err = sstore.UpdateClientOpenAIOpts(ctx, *aiOpts)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai model: %v", err)
		}
	}
	if maxTokensStr, found := pk.Kwargs["openaimaxtokens"]; found {
		maxTokens, err := strconv.Atoi(maxTokensStr)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai maxtokens, invalid number: %v", err)
		}
		if maxTokens < 0 || maxTokens > 1000000 {
			return nil, fmt.Errorf("error updating client openai maxtokens, out of range: %d", maxTokens)
		}
		varsUpdated = append(varsUpdated, "openaimaxtokens")
		aiOpts := clientData.OpenAIOpts
		if aiOpts == nil {
			aiOpts = &sstore.OpenAIOptsType{}
			clientData.OpenAIOpts = aiOpts
		}
		aiOpts.MaxTokens = maxTokens
		err = sstore.UpdateClientOpenAIOpts(ctx, *aiOpts)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai maxtokens: %v", err)
		}
	}
	if maxChoicesStr, found := pk.Kwargs["openaimaxchoices"]; found {
		maxChoices, err := strconv.Atoi(maxChoicesStr)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai maxchoices, invalid number: %v", err)
		}
		if maxChoices < 0 || maxChoices > 10 {
			return nil, fmt.Errorf("error updating client openai maxchoices, out of range: %d", maxChoices)
		}
		varsUpdated = append(varsUpdated, "openaimaxchoices")
		aiOpts := clientData.OpenAIOpts
		if aiOpts == nil {
			aiOpts = &sstore.OpenAIOptsType{}
			clientData.OpenAIOpts = aiOpts
		}
		aiOpts.MaxChoices = maxChoices
		err = sstore.UpdateClientOpenAIOpts(ctx, *aiOpts)
		if err != nil {
			return nil, fmt.Errorf("error updating client openai maxchoices: %v", err)
		}
	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/client:set requires a value to set: %s", formatStrs([]string{"termfontsize", "openaiapitoken", "openaimodel", "openaimaxtokens", "openaimaxchoices"}, "or", false))
	}
	clientData, err = sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve updated client data: %v", err)
	}
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("client updated %s", formatStrs(varsUpdated, "and", false)),
			TimeoutMs: 2000,
		},
		ClientData: clientData,
	}
	return update, nil
}

func ClientShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	dbVersion, err := sstore.GetDBVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve db version: %v\n", err)
	}
	clientVersion := "-"
	if pk.UIContext != nil && pk.UIContext.Build != "" {
		clientVersion = pk.UIContext.Build
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "userid", clientData.UserId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "clientid", clientData.ClientId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "telemetry", boolToStr(clientData.ClientOpts.NoTelemetry, "off", "on")))
	buf.WriteString(fmt.Sprintf("  %-15s %d\n", "db-version", dbVersion))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "client-version", clientVersion))
	buf.WriteString(fmt.Sprintf("  %-15s %s %s\n", "server-version", scbase.WaveVersion, scbase.BuildTime))
	buf.WriteString(fmt.Sprintf("  %-15s %s (%s)\n", "arch", scbase.ClientArch(), scbase.MacOSRelease()))
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("client info"),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func TelemetryCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/telemetry requires a subcommand: %s", formatStrs([]string{"show", "on", "off", "send"}, "or", false))
}

func setNoTelemetry(ctx context.Context, clientData *sstore.ClientData, noTelemetryVal bool) error {
	clientOpts := clientData.ClientOpts
	clientOpts.NoTelemetry = noTelemetryVal
	err := sstore.SetClientOpts(ctx, clientOpts)
	if err != nil {
		return fmt.Errorf("error trying to update client telemetry: %v", err)
	}
	log.Printf("client no-telemetry setting updated to %v\n", noTelemetryVal)
	go func() {
		err := pcloud.SendNoTelemetryUpdate(ctx, clientOpts.NoTelemetry)
		if err != nil {
			log.Printf("[error] sending no-telemetry update: %v\n", err)
			log.Printf("note that telemetry update has still taken effect locally, and will be respected by the client\n")
		}
	}()
	return nil
}

func TelemetryOnCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	if !clientData.ClientOpts.NoTelemetry {
		return sstore.InfoMsgUpdate("telemetry is already on"), nil
	}
	err = setNoTelemetry(ctx, clientData, false)
	if err != nil {
		return nil, err
	}
	go func() {
		err := pcloud.SendTelemetry(ctx, false)
		if err != nil {
			// ignore error, but log
			log.Printf("[error] sending telemetry update (in /telemetry:on): %v\n", err)
		}
	}()
	clientData, err = sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve updated client data: %v", err)
	}
	update := sstore.InfoMsgUpdate("telemetry is now on")
	update.ClientData = clientData
	return update, nil
}

func TelemetryOffCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	if clientData.ClientOpts.NoTelemetry {
		return sstore.InfoMsgUpdate("telemetry is already off"), nil
	}
	err = setNoTelemetry(ctx, clientData, true)
	if err != nil {
		return nil, err
	}
	clientData, err = sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve updated client data: %v", err)
	}
	update := sstore.InfoMsgUpdate("telemetry is now off")
	update.ClientData = clientData
	return update, nil
}

func TelemetryShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "telemetry", boolToStr(clientData.ClientOpts.NoTelemetry, "off", "on")))
	update := &sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("telemetry info"),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func TelemetrySendCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	clientData, err := sstore.EnsureClientData(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve client data: %v", err)
	}
	force := resolveBool(pk.Kwargs["force"], false)
	if clientData.ClientOpts.NoTelemetry && !force {
		return nil, fmt.Errorf("cannot send telemetry, telemetry is off.  pass force=1 to force the send, or turn on telemetry with /telemetry:on")
	}
	err = pcloud.SendTelemetry(ctx, force)
	if err != nil {
		return nil, fmt.Errorf("failed to send telemetry: %v", err)
	}
	return sstore.InfoMsgUpdate("telemetry sent"), nil
}

func formatTermOpts(termOpts sstore.TermOpts) string {
	if termOpts.Cols == 0 {
		return "???"
	}
	rtnStr := fmt.Sprintf("%dx%d", termOpts.Rows, termOpts.Cols)
	if termOpts.FlexRows {
		rtnStr += " flexrows"
	}
	if termOpts.MaxPtySize > 0 {
		rtnStr += " maxbuf=" + scbase.NumFormatB2(termOpts.MaxPtySize)
	}
	return rtnStr
}

type ColMeta struct {
	Title   string
	MinCols int
	MaxCols int
}

func toInterfaceArr(sarr []string) []interface{} {
	rtn := make([]interface{}, len(sarr))
	for idx, s := range sarr {
		rtn[idx] = s
	}
	return rtn
}

func formatTextTable(totalCols int, data [][]string, colMeta []ColMeta) []string {
	numCols := len(colMeta)
	maxColLen := make([]int, len(colMeta))
	for i, cm := range colMeta {
		maxColLen[i] = cm.MinCols
	}
	for _, row := range data {
		for i := 0; i < numCols && i < len(row); i++ {
			dlen := len(row[i])
			if dlen > maxColLen[i] {
				maxColLen[i] = dlen
			}
		}
	}
	fmtStr := ""
	for idx, clen := range maxColLen {
		if idx != 0 {
			fmtStr += " "
		}
		fmtStr += fmt.Sprintf("%%%ds", clen)
	}
	var rtn []string
	for _, row := range data {
		sval := fmt.Sprintf(fmtStr, toInterfaceArr(row)...)
		rtn = append(rtn, sval)
	}
	return rtn
}

func isValidInScope(scopeName string, varName string) bool {
	for _, varScope := range SetVarScopes {
		if varScope.ScopeName == scopeName {
			return utilfn.ContainsStr(varScope.VarNames, varName)
		}
	}
	return false
}

// returns (is-valid, scope, name)
// TODO write a full resolver to allow for indexed arguments.  e.g. session[1].screen[1].screen.pterm="25x80"
func resolveSetArg(argName string) (bool, string, string) {
	dotIdx := strings.Index(argName, ".")
	if dotIdx == -1 {
		argName = SetVarNameMap[argName]
		dotIdx = strings.Index(argName, ".")
	}
	if argName == "" {
		return false, "", ""
	}
	scopeName := argName[0:dotIdx]
	varName := argName[dotIdx+1:]
	if !isValidInScope(scopeName, varName) {
		return false, "", ""
	}
	return true, scopeName, varName
}
