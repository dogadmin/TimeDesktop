// DesktopTime — Windows 多时区桌面悬浮窗（单文件、纯 Go、无 CGO）
//
// 设计要点：
//   - 只用 Win32 + GDI（via golang.org/x/sys/windows 的 LazyProc），不依赖 CGO。
//   - 无边框 + WS_EX_TOPMOST 置顶 + WS_EX_LAYERED 透明度 + WS_EX_TOOLWINDOW 不进 Alt+Tab。
//   - 本机时区：读 HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation\TimeZoneKeyName
//     得到 Windows 时区键（如 "China Standard Time"），内嵌 CLDR 映射表换成 IANA
//     （如 "Asia/Shanghai"），再在城市表里取真正的地名（上海 / Shanghai）。
//   - 右键菜单原生 TrackPopupMenu；所有城市 / 大洲 / 菜单项都是中英双语，菜单里可切换。
//   - 拖动：ReleaseCapture + WM_NCLBUTTONDOWN(HTCAPTION)。
//   - 持久化：golang.org/x/sys/windows/registry 写 HKCU\Software\DesktopTime。
//   - 联网校时：worldtimeapi.org 为主、Google Date 头为备；3 秒超时，失败静默回落。
//   - 消息循环必须 runtime.LockOSThread 钉在单一 OS 线程上。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ============================================================================
// 应用常量
// ============================================================================

const (
	appClassName = "DesktopTimeClock"
	regSubKey    = `Software\DesktopTime`
	maxZones     = 10
	timerRefresh = 1
)

// ============================================================================
// Win32 常量 / DLL / 结构
// ============================================================================

const (
	ws_POPUP        = 0x80000000
	ws_VISIBLE      = 0x10000000
	wsEx_TOPMOST    = 0x00000008
	wsEx_LAYERED    = 0x00080000
	wsEx_TOOLWINDOW = 0x00000080

	cs_HREDRAW = 0x0002
	cs_VREDRAW = 0x0001

	wmDESTROY       = 0x0002
	wmPAINT         = 0x000F
	wmCLOSE         = 0x0010
	wmERASEBKGND    = 0x0014
	wmSETCURSOR     = 0x0020
	wmTIMER         = 0x0113
	wmLBUTTONDOWN   = 0x0201
	wmRBUTTONUP     = 0x0205
	wmNCLBUTTONDOWN = 0x00A1
	wmEXITSIZEMOVE  = 0x0232
	htCAPTION       = 2

	mf_STRING       = 0x00000000
	mf_POPUP        = 0x00000010
	mf_GRAYED       = 0x00000001
	mf_CHECKED      = 0x00000008
	mf_SEPARATOR    = 0x00000800
	mb_ICONERROR    = 0x00000010
	tpm_LEFTALIGN   = 0x0000
	tpm_RIGHTBUTTON = 0x0002
	tpm_RETURNCMD   = 0x0100

	sw_SHOWNOACTIVATE = 4
	lwa_ALPHA         = 0x00000002

	swp_NOZORDER   = 0x0004
	swp_NOACTIVATE = 0x0010

	fw_NORMAL         = 400
	defaultCharset    = 1
	outDefaultPrecis  = 0
	clipDefaultPrecis = 0
	cleartypeQuality  = 5
	defaultPitch      = 0
	bkModeTransparent = 1
	idcARROW          = 32512
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassEx       = user32.NewProc("RegisterClassExW")
	procCreateWindowEx        = user32.NewProc("CreateWindowExW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procUpdateWindow          = user32.NewProc("UpdateWindow")
	procDefWindowProc         = user32.NewProc("DefWindowProcW")
	procGetMessage            = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessage       = user32.NewProc("DispatchMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procSetLayeredWindowAttrs = user32.NewProc("SetLayeredWindowAttributes")
	procSetTimer              = user32.NewProc("SetTimer")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procInvalidateRect        = user32.NewProc("InvalidateRect")
	procGetClientRect         = user32.NewProc("GetClientRect")
	procGetWindowRect         = user32.NewProc("GetWindowRect")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procReleaseCapture        = user32.NewProc("ReleaseCapture")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procFillRect              = user32.NewProc("FillRect")
	procBeginPaint            = user32.NewProc("BeginPaint")
	procEndPaint              = user32.NewProc("EndPaint")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	procSetCursor             = user32.NewProc("SetCursor")
	procGetDC                 = user32.NewProc("GetDC")
	procReleaseDC             = user32.NewProc("ReleaseDC")
	procMessageBoxW           = user32.NewProc("MessageBoxW")

	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procTextOutW              = gdi32.NewProc("TextOutW")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}
type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
type msgT struct {
	Hwnd     windows.HWND
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}
type paintStruct struct {
	Hdc         windows.Handle
	Erase       int32
	RcPaint     rect
	Restore     int32
	IncUpdate   int32
	RgbReserved [32]byte
}
type sizeStruct struct{ CX, CY int32 }

func rgb(r, g, b uint8) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }
func u16(s string) *uint16      { p, _ := syscall.UTF16PtrFromString(s); return p }

// u16slice：UTF-8 → UTF-16，去掉末尾 NUL，供 TextOutW / GetTextExtentPoint32W 用（这俩要 cchString）
func u16slice(s string) []uint16 {
	p, _ := syscall.UTF16FromString(s)
	for len(p) > 0 && p[len(p)-1] == 0 {
		p = p[:len(p)-1]
	}
	return p
}

// fatal：-H windowsgui 下 stderr / panic 都看不见，只能弹 MessageBox。
func fatal(msg string) {
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(u16(msg))),
		uintptr(unsafe.Pointer(u16("DesktopTime"))),
		mb_ICONERROR)
	os.Exit(1)
}

// ============================================================================
// 时区数据（IANA 分洲 + 中英双语；菜单/显示都走这张表）
// ============================================================================

type tzCity struct {
	tz string // IANA
	en string
	cn string
}
type tzRegion struct {
	en     string
	cn     string
	cities []tzCity
}

var regions = []tzRegion{
	{"Africa", "非洲", []tzCity{
		{"Africa/Abidjan", "Abidjan", "阿比让"},
		{"Africa/Accra", "Accra", "阿克拉"},
		{"Africa/Addis_Ababa", "Addis Ababa", "亚的斯亚贝巴"},
		{"Africa/Algiers", "Algiers", "阿尔及尔"},
		{"Africa/Asmara", "Asmara", "阿斯马拉"},
		{"Africa/Bamako", "Bamako", "巴马科"},
		{"Africa/Bangui", "Bangui", "班吉"},
		{"Africa/Banjul", "Banjul", "班珠尔"},
		{"Africa/Bissau", "Bissau", "比绍"},
		{"Africa/Blantyre", "Blantyre", "布兰太尔"},
		{"Africa/Brazzaville", "Brazzaville", "布拉柴维尔"},
		{"Africa/Bujumbura", "Bujumbura", "布琼布拉"},
		{"Africa/Cairo", "Cairo", "开罗"},
		{"Africa/Casablanca", "Casablanca", "卡萨布兰卡"},
		{"Africa/Ceuta", "Ceuta", "休达"},
		{"Africa/Conakry", "Conakry", "科纳克里"},
		{"Africa/Dakar", "Dakar", "达喀尔"},
		{"Africa/Dar_es_Salaam", "Dar es Salaam", "达累斯萨拉姆"},
		{"Africa/Djibouti", "Djibouti", "吉布提"},
		{"Africa/Douala", "Douala", "杜阿拉"},
		{"Africa/El_Aaiun", "El Aaiun", "阿尤恩"},
		{"Africa/Freetown", "Freetown", "弗里敦"},
		{"Africa/Gaborone", "Gaborone", "哈博罗内"},
		{"Africa/Harare", "Harare", "哈拉雷"},
		{"Africa/Johannesburg", "Johannesburg", "约翰内斯堡"},
		{"Africa/Juba", "Juba", "朱巴"},
		{"Africa/Kampala", "Kampala", "坎帕拉"},
		{"Africa/Khartoum", "Khartoum", "喀土穆"},
		{"Africa/Kigali", "Kigali", "基加利"},
		{"Africa/Kinshasa", "Kinshasa", "金沙萨"},
		{"Africa/Lagos", "Lagos", "拉各斯"},
		{"Africa/Libreville", "Libreville", "利伯维尔"},
		{"Africa/Lome", "Lome", "洛美"},
		{"Africa/Luanda", "Luanda", "罗安达"},
		{"Africa/Lubumbashi", "Lubumbashi", "卢本巴希"},
		{"Africa/Lusaka", "Lusaka", "卢萨卡"},
		{"Africa/Malabo", "Malabo", "马拉博"},
		{"Africa/Maputo", "Maputo", "马普托"},
		{"Africa/Maseru", "Maseru", "马塞卢"},
		{"Africa/Mbabane", "Mbabane", "姆巴巴内"},
		{"Africa/Mogadishu", "Mogadishu", "摩加迪沙"},
		{"Africa/Monrovia", "Monrovia", "蒙罗维亚"},
		{"Africa/Nairobi", "Nairobi", "内罗毕"},
		{"Africa/Ndjamena", "Ndjamena", "恩贾梅纳"},
		{"Africa/Niamey", "Niamey", "尼亚美"},
		{"Africa/Nouakchott", "Nouakchott", "努瓦克肖特"},
		{"Africa/Ouagadougou", "Ouagadougou", "瓦加杜古"},
		{"Africa/Porto-Novo", "Porto-Novo", "波多诺伏"},
		{"Africa/Sao_Tome", "Sao Tome", "圣多美"},
		{"Africa/Tripoli", "Tripoli", "的黎波里"},
		{"Africa/Tunis", "Tunis", "突尼斯"},
		{"Africa/Windhoek", "Windhoek", "温得和克"},
	}},
	{"America", "美洲", []tzCity{
		{"America/Adak", "Adak", "阿达克"},
		{"America/Anchorage", "Anchorage", "安克雷奇"},
		{"America/Anguilla", "Anguilla", "安圭拉"},
		{"America/Antigua", "Antigua", "安提瓜"},
		{"America/Araguaina", "Araguaina", "阿拉瓜伊纳"},
		{"America/Argentina/Buenos_Aires", "Buenos Aires", "布宜诺斯艾利斯"},
		{"America/Argentina/Cordoba", "Cordoba (AR)", "科尔多瓦"},
		{"America/Argentina/Mendoza", "Mendoza", "门多萨"},
		{"America/Argentina/Ushuaia", "Ushuaia", "乌斯怀亚"},
		{"America/Aruba", "Aruba", "阿鲁巴"},
		{"America/Asuncion", "Asuncion", "亚松森"},
		{"America/Atikokan", "Atikokan", "阿蒂科肯"},
		{"America/Bahia", "Bahia", "巴伊亚"},
		{"America/Barbados", "Barbados", "巴巴多斯"},
		{"America/Belem", "Belem", "贝伦"},
		{"America/Belize", "Belize", "伯利兹"},
		{"America/Boa_Vista", "Boa Vista", "博阿维斯塔"},
		{"America/Bogota", "Bogota", "波哥大"},
		{"America/Boise", "Boise", "博伊西"},
		{"America/Cambridge_Bay", "Cambridge Bay", "剑桥湾"},
		{"America/Campo_Grande", "Campo Grande", "大坎普"},
		{"America/Cancun", "Cancun", "坎昆"},
		{"America/Caracas", "Caracas", "加拉加斯"},
		{"America/Cayenne", "Cayenne", "卡宴"},
		{"America/Cayman", "Cayman", "开曼"},
		{"America/Chicago", "Chicago", "芝加哥"},
		{"America/Chihuahua", "Chihuahua", "奇瓦瓦"},
		{"America/Costa_Rica", "Costa Rica", "哥斯达黎加"},
		{"America/Creston", "Creston", "克雷斯顿"},
		{"America/Cuiaba", "Cuiaba", "库亚巴"},
		{"America/Curacao", "Curacao", "库拉索"},
		{"America/Danmarkshavn", "Danmarkshavn", "丹麦港"},
		{"America/Dawson", "Dawson", "道森"},
		{"America/Dawson_Creek", "Dawson Creek", "道森溪"},
		{"America/Denver", "Denver", "丹佛"},
		{"America/Detroit", "Detroit", "底特律"},
		{"America/Dominica", "Dominica", "多米尼克"},
		{"America/Edmonton", "Edmonton", "埃德蒙顿"},
		{"America/Eirunepe", "Eirunepe", "埃鲁内佩"},
		{"America/El_Salvador", "El Salvador", "萨尔瓦多"},
		{"America/Fort_Nelson", "Fort Nelson", "尼尔森堡"},
		{"America/Fortaleza", "Fortaleza", "福塔雷萨"},
		{"America/Glace_Bay", "Glace Bay", "格莱斯贝"},
		{"America/Goose_Bay", "Goose Bay", "古斯贝"},
		{"America/Grand_Turk", "Grand Turk", "大特克"},
		{"America/Grenada", "Grenada", "格林纳达"},
		{"America/Guadeloupe", "Guadeloupe", "瓜德罗普"},
		{"America/Guatemala", "Guatemala", "危地马拉"},
		{"America/Guayaquil", "Guayaquil", "瓜亚基尔"},
		{"America/Guyana", "Guyana", "圭亚那"},
		{"America/Halifax", "Halifax", "哈利法克斯"},
		{"America/Havana", "Havana", "哈瓦那"},
		{"America/Hermosillo", "Hermosillo", "埃莫西约"},
		{"America/Indiana/Indianapolis", "Indianapolis", "印第安纳波利斯"},
		{"America/Inuvik", "Inuvik", "伊努维克"},
		{"America/Iqaluit", "Iqaluit", "伊卡卢伊特"},
		{"America/Jamaica", "Jamaica", "牙买加"},
		{"America/Juneau", "Juneau", "朱诺"},
		{"America/Kentucky/Louisville", "Louisville", "路易维尔"},
		{"America/La_Paz", "La Paz", "拉巴斯"},
		{"America/Lima", "Lima", "利马"},
		{"America/Los_Angeles", "Los Angeles", "洛杉矶"},
		{"America/Maceio", "Maceio", "马塞约"},
		{"America/Managua", "Managua", "马那瓜"},
		{"America/Manaus", "Manaus", "马瑙斯"},
		{"America/Martinique", "Martinique", "马提尼克"},
		{"America/Matamoros", "Matamoros", "马塔莫罗斯"},
		{"America/Mazatlan", "Mazatlan", "马萨特兰"},
		{"America/Menominee", "Menominee", "梅诺米尼"},
		{"America/Merida", "Merida", "梅里达"},
		{"America/Metlakatla", "Metlakatla", "梅特拉卡特拉"},
		{"America/Mexico_City", "Mexico City", "墨西哥城"},
		{"America/Miquelon", "Miquelon", "密克隆"},
		{"America/Moncton", "Moncton", "蒙克顿"},
		{"America/Monterrey", "Monterrey", "蒙特雷"},
		{"America/Montevideo", "Montevideo", "蒙得维的亚"},
		{"America/Nassau", "Nassau", "拿骚"},
		{"America/New_York", "New York", "纽约"},
		{"America/Nome", "Nome", "诺姆"},
		{"America/Noronha", "Noronha", "诺罗尼亚"},
		{"America/Nuuk", "Nuuk", "努克"},
		{"America/Ojinaga", "Ojinaga", "奥希纳加"},
		{"America/Panama", "Panama", "巴拿马"},
		{"America/Paramaribo", "Paramaribo", "帕拉马里博"},
		{"America/Phoenix", "Phoenix", "凤凰城"},
		{"America/Port-au-Prince", "Port-au-Prince", "太子港"},
		{"America/Port_of_Spain", "Port of Spain", "西班牙港"},
		{"America/Porto_Velho", "Porto Velho", "波多韦柳"},
		{"America/Puerto_Rico", "Puerto Rico", "波多黎各"},
		{"America/Punta_Arenas", "Punta Arenas", "蓬塔阿雷纳斯"},
		{"America/Rankin_Inlet", "Rankin Inlet", "兰金湾"},
		{"America/Recife", "Recife", "累西腓"},
		{"America/Regina", "Regina", "里贾纳"},
		{"America/Resolute", "Resolute", "雷索卢特"},
		{"America/Rio_Branco", "Rio Branco", "里约布朗库"},
		{"America/Santarem", "Santarem", "圣塔伦"},
		{"America/Santiago", "Santiago", "圣地亚哥"},
		{"America/Santo_Domingo", "Santo Domingo", "圣多明各"},
		{"America/Sao_Paulo", "Sao Paulo", "圣保罗"},
		{"America/Scoresbysund", "Scoresbysund", "斯科斯比松"},
		{"America/Sitka", "Sitka", "锡特卡"},
		{"America/St_Johns", "St Johns", "圣约翰"},
		{"America/Swift_Current", "Swift Current", "斯威夫特卡伦特"},
		{"America/Tegucigalpa", "Tegucigalpa", "特古西加尔巴"},
		{"America/Thule", "Thule", "图勒"},
		{"America/Tijuana", "Tijuana", "蒂华纳"},
		{"America/Toronto", "Toronto", "多伦多"},
		{"America/Vancouver", "Vancouver", "温哥华"},
		{"America/Whitehorse", "Whitehorse", "白马"},
		{"America/Winnipeg", "Winnipeg", "温尼伯"},
		{"America/Yakutat", "Yakutat", "亚库塔特"},
	}},
	{"Antarctica", "南极洲", []tzCity{
		{"Antarctica/Casey", "Casey", "凯西站"},
		{"Antarctica/Davis", "Davis", "戴维斯站"},
		{"Antarctica/DumontDUrville", "Dumont d'Urville", "迪蒙迪维尔"},
		{"Antarctica/Macquarie", "Macquarie", "麦夸里"},
		{"Antarctica/Mawson", "Mawson", "莫森站"},
		{"Antarctica/McMurdo", "McMurdo", "麦克默多"},
		{"Antarctica/Palmer", "Palmer", "帕默站"},
		{"Antarctica/Rothera", "Rothera", "罗瑟拉"},
		{"Antarctica/Syowa", "Syowa", "昭和站"},
		{"Antarctica/Troll", "Troll", "特罗尔站"},
		{"Antarctica/Vostok", "Vostok", "沃斯托克"},
	}},
	{"Asia", "亚洲", []tzCity{
		{"Asia/Aden", "Aden", "亚丁"},
		{"Asia/Almaty", "Almaty", "阿拉木图"},
		{"Asia/Amman", "Amman", "安曼"},
		{"Asia/Anadyr", "Anadyr", "阿纳德尔"},
		{"Asia/Aqtau", "Aqtau", "阿克套"},
		{"Asia/Aqtobe", "Aqtobe", "阿克托别"},
		{"Asia/Ashgabat", "Ashgabat", "阿什哈巴德"},
		{"Asia/Atyrau", "Atyrau", "阿特劳"},
		{"Asia/Baghdad", "Baghdad", "巴格达"},
		{"Asia/Bahrain", "Bahrain", "巴林"},
		{"Asia/Baku", "Baku", "巴库"},
		{"Asia/Bangkok", "Bangkok", "曼谷"},
		{"Asia/Barnaul", "Barnaul", "巴尔瑙尔"},
		{"Asia/Beirut", "Beirut", "贝鲁特"},
		{"Asia/Bishkek", "Bishkek", "比什凯克"},
		{"Asia/Brunei", "Brunei", "文莱"},
		{"Asia/Chita", "Chita", "赤塔"},
		{"Asia/Choibalsan", "Choibalsan", "乔巴山"},
		{"Asia/Colombo", "Colombo", "科伦坡"},
		{"Asia/Damascus", "Damascus", "大马士革"},
		{"Asia/Dhaka", "Dhaka", "达卡"},
		{"Asia/Dili", "Dili", "帝力"},
		{"Asia/Dubai", "Dubai", "迪拜"},
		{"Asia/Dushanbe", "Dushanbe", "杜尚别"},
		{"Asia/Famagusta", "Famagusta", "法马古斯塔"},
		{"Asia/Gaza", "Gaza", "加沙"},
		{"Asia/Hebron", "Hebron", "希伯伦"},
		{"Asia/Ho_Chi_Minh", "Ho Chi Minh", "胡志明市"},
		{"Asia/Hong_Kong", "Hong Kong", "香港"},
		{"Asia/Hovd", "Hovd", "科布多"},
		{"Asia/Irkutsk", "Irkutsk", "伊尔库茨克"},
		{"Asia/Jakarta", "Jakarta", "雅加达"},
		{"Asia/Jayapura", "Jayapura", "查亚普拉"},
		{"Asia/Jerusalem", "Jerusalem", "耶路撒冷"},
		{"Asia/Kabul", "Kabul", "喀布尔"},
		{"Asia/Kamchatka", "Kamchatka", "堪察加"},
		{"Asia/Karachi", "Karachi", "卡拉奇"},
		{"Asia/Kathmandu", "Kathmandu", "加德满都"},
		{"Asia/Khandyga", "Khandyga", "汉德加"},
		{"Asia/Kolkata", "Kolkata", "加尔各答"},
		{"Asia/Krasnoyarsk", "Krasnoyarsk", "克拉斯诺亚尔斯克"},
		{"Asia/Kuala_Lumpur", "Kuala Lumpur", "吉隆坡"},
		{"Asia/Kuching", "Kuching", "古晋"},
		{"Asia/Kuwait", "Kuwait", "科威特"},
		{"Asia/Macau", "Macau", "澳门"},
		{"Asia/Magadan", "Magadan", "马加丹"},
		{"Asia/Makassar", "Makassar", "望加锡"},
		{"Asia/Manila", "Manila", "马尼拉"},
		{"Asia/Muscat", "Muscat", "马斯喀特"},
		{"Asia/Nicosia", "Nicosia", "尼科西亚"},
		{"Asia/Novokuznetsk", "Novokuznetsk", "新库兹涅茨克"},
		{"Asia/Novosibirsk", "Novosibirsk", "新西伯利亚"},
		{"Asia/Omsk", "Omsk", "鄂木斯克"},
		{"Asia/Oral", "Oral", "乌拉尔斯克"},
		{"Asia/Phnom_Penh", "Phnom Penh", "金边"},
		{"Asia/Pontianak", "Pontianak", "坤甸"},
		{"Asia/Pyongyang", "Pyongyang", "平壤"},
		{"Asia/Qatar", "Qatar", "卡塔尔"},
		{"Asia/Qostanay", "Qostanay", "科斯塔奈"},
		{"Asia/Qyzylorda", "Qyzylorda", "克孜勒奥尔达"},
		{"Asia/Riyadh", "Riyadh", "利雅得"},
		{"Asia/Sakhalin", "Sakhalin", "萨哈林"},
		{"Asia/Samarkand", "Samarkand", "撒马尔罕"},
		{"Asia/Seoul", "Seoul", "首尔"},
		{"Asia/Shanghai", "Shanghai", "上海"},
		{"Asia/Singapore", "Singapore", "新加坡"},
		{"Asia/Srednekolymsk", "Srednekolymsk", "中科雷姆斯克"},
		{"Asia/Taipei", "Taipei", "台北"},
		{"Asia/Tashkent", "Tashkent", "塔什干"},
		{"Asia/Tbilisi", "Tbilisi", "第比利斯"},
		{"Asia/Tehran", "Tehran", "德黑兰"},
		{"Asia/Thimphu", "Thimphu", "廷布"},
		{"Asia/Tokyo", "Tokyo", "东京"},
		{"Asia/Tomsk", "Tomsk", "托木斯克"},
		{"Asia/Ulaanbaatar", "Ulaanbaatar", "乌兰巴托"},
		{"Asia/Urumqi", "Urumqi", "乌鲁木齐"},
		{"Asia/Ust-Nera", "Ust-Nera", "乌斯季涅拉"},
		{"Asia/Vientiane", "Vientiane", "万象"},
		{"Asia/Vladivostok", "Vladivostok", "符拉迪沃斯托克"},
		{"Asia/Yakutsk", "Yakutsk", "雅库茨克"},
		{"Asia/Yangon", "Yangon", "仰光"},
		{"Asia/Yekaterinburg", "Yekaterinburg", "叶卡捷琳堡"},
		{"Asia/Yerevan", "Yerevan", "埃里温"},
	}},
	{"Atlantic", "大西洋", []tzCity{
		{"Atlantic/Azores", "Azores", "亚速尔"},
		{"Atlantic/Bermuda", "Bermuda", "百慕大"},
		{"Atlantic/Canary", "Canary", "加那利"},
		{"Atlantic/Cape_Verde", "Cape Verde", "佛得角"},
		{"Atlantic/Faroe", "Faroe", "法罗"},
		{"Atlantic/Madeira", "Madeira", "马德拉"},
		{"Atlantic/Reykjavik", "Reykjavik", "雷克雅未克"},
		{"Atlantic/South_Georgia", "South Georgia", "南乔治亚"},
		{"Atlantic/Stanley", "Stanley", "斯坦利"},
	}},
	{"Australia", "大洋洲", []tzCity{
		{"Australia/Adelaide", "Adelaide", "阿德莱德"},
		{"Australia/Brisbane", "Brisbane", "布里斯班"},
		{"Australia/Broken_Hill", "Broken Hill", "布罗肯希尔"},
		{"Australia/Darwin", "Darwin", "达尔文"},
		{"Australia/Eucla", "Eucla", "尤克拉"},
		{"Australia/Hobart", "Hobart", "霍巴特"},
		{"Australia/Lindeman", "Lindeman", "林德曼"},
		{"Australia/Lord_Howe", "Lord Howe", "豪勋爵"},
		{"Australia/Melbourne", "Melbourne", "墨尔本"},
		{"Australia/Perth", "Perth", "珀斯"},
		{"Australia/Sydney", "Sydney", "悉尼"},
	}},
	{"Europe", "欧洲", []tzCity{
		{"Europe/Amsterdam", "Amsterdam", "阿姆斯特丹"},
		{"Europe/Andorra", "Andorra", "安道尔"},
		{"Europe/Astrakhan", "Astrakhan", "阿斯特拉罕"},
		{"Europe/Athens", "Athens", "雅典"},
		{"Europe/Belgrade", "Belgrade", "贝尔格莱德"},
		{"Europe/Berlin", "Berlin", "柏林"},
		{"Europe/Brussels", "Brussels", "布鲁塞尔"},
		{"Europe/Bucharest", "Bucharest", "布加勒斯特"},
		{"Europe/Budapest", "Budapest", "布达佩斯"},
		{"Europe/Chisinau", "Chisinau", "基希讷乌"},
		{"Europe/Copenhagen", "Copenhagen", "哥本哈根"},
		{"Europe/Dublin", "Dublin", "都柏林"},
		{"Europe/Gibraltar", "Gibraltar", "直布罗陀"},
		{"Europe/Helsinki", "Helsinki", "赫尔辛基"},
		{"Europe/Istanbul", "Istanbul", "伊斯坦布尔"},
		{"Europe/Kaliningrad", "Kaliningrad", "加里宁格勒"},
		{"Europe/Kirov", "Kirov", "基洛夫"},
		{"Europe/Kyiv", "Kyiv", "基辅"},
		{"Europe/Lisbon", "Lisbon", "里斯本"},
		{"Europe/London", "London", "伦敦"},
		{"Europe/Luxembourg", "Luxembourg", "卢森堡"},
		{"Europe/Madrid", "Madrid", "马德里"},
		{"Europe/Malta", "Malta", "马耳他"},
		{"Europe/Minsk", "Minsk", "明斯克"},
		{"Europe/Monaco", "Monaco", "摩纳哥"},
		{"Europe/Moscow", "Moscow", "莫斯科"},
		{"Europe/Oslo", "Oslo", "奥斯陆"},
		{"Europe/Paris", "Paris", "巴黎"},
		{"Europe/Prague", "Prague", "布拉格"},
		{"Europe/Riga", "Riga", "里加"},
		{"Europe/Rome", "Rome", "罗马"},
		{"Europe/Samara", "Samara", "萨马拉"},
		{"Europe/Saratov", "Saratov", "萨拉托夫"},
		{"Europe/Simferopol", "Simferopol", "辛菲罗波尔"},
		{"Europe/Sofia", "Sofia", "索非亚"},
		{"Europe/Stockholm", "Stockholm", "斯德哥尔摩"},
		{"Europe/Tallinn", "Tallinn", "塔林"},
		{"Europe/Tirane", "Tirane", "地拉那"},
		{"Europe/Ulyanovsk", "Ulyanovsk", "乌里扬诺夫斯克"},
		{"Europe/Vienna", "Vienna", "维也纳"},
		{"Europe/Vilnius", "Vilnius", "维尔纽斯"},
		{"Europe/Volgograd", "Volgograd", "伏尔加格勒"},
		{"Europe/Warsaw", "Warsaw", "华沙"},
		{"Europe/Zurich", "Zurich", "苏黎世"},
	}},
	{"Indian", "印度洋", []tzCity{
		{"Indian/Antananarivo", "Antananarivo", "塔那那利佛"},
		{"Indian/Chagos", "Chagos", "查戈斯"},
		{"Indian/Christmas", "Christmas", "圣诞岛"},
		{"Indian/Cocos", "Cocos", "科科斯"},
		{"Indian/Comoro", "Comoro", "科摩罗"},
		{"Indian/Kerguelen", "Kerguelen", "凯尔盖朗"},
		{"Indian/Mahe", "Mahe", "马埃"},
		{"Indian/Maldives", "Maldives", "马尔代夫"},
		{"Indian/Mauritius", "Mauritius", "毛里求斯"},
		{"Indian/Mayotte", "Mayotte", "马约特"},
		{"Indian/Reunion", "Reunion", "留尼汪"},
	}},
	{"Pacific", "太平洋", []tzCity{
		{"Pacific/Apia", "Apia", "阿皮亚"},
		{"Pacific/Auckland", "Auckland", "奥克兰"},
		{"Pacific/Bougainville", "Bougainville", "布干维尔"},
		{"Pacific/Chatham", "Chatham", "查塔姆"},
		{"Pacific/Chuuk", "Chuuk", "丘克"},
		{"Pacific/Easter", "Easter", "复活节岛"},
		{"Pacific/Efate", "Efate", "埃法特"},
		{"Pacific/Fakaofo", "Fakaofo", "法考福"},
		{"Pacific/Fiji", "Fiji", "斐济"},
		{"Pacific/Funafuti", "Funafuti", "富纳富提"},
		{"Pacific/Galapagos", "Galapagos", "加拉帕戈斯"},
		{"Pacific/Gambier", "Gambier", "甘比尔"},
		{"Pacific/Guadalcanal", "Guadalcanal", "瓜达尔卡纳尔"},
		{"Pacific/Guam", "Guam", "关岛"},
		{"Pacific/Honolulu", "Honolulu", "檀香山"},
		{"Pacific/Kanton", "Kanton", "坎顿岛"},
		{"Pacific/Kiritimati", "Kiritimati", "基里蒂马蒂"},
		{"Pacific/Kosrae", "Kosrae", "科斯雷"},
		{"Pacific/Kwajalein", "Kwajalein", "夸贾林"},
		{"Pacific/Majuro", "Majuro", "马朱罗"},
		{"Pacific/Marquesas", "Marquesas", "马克萨斯"},
		{"Pacific/Midway", "Midway", "中途岛"},
		{"Pacific/Nauru", "Nauru", "瑙鲁"},
		{"Pacific/Niue", "Niue", "纽埃"},
		{"Pacific/Norfolk", "Norfolk", "诺福克"},
		{"Pacific/Noumea", "Noumea", "努美阿"},
		{"Pacific/Pago_Pago", "Pago Pago", "帕果帕果"},
		{"Pacific/Palau", "Palau", "帕劳"},
		{"Pacific/Pitcairn", "Pitcairn", "皮特凯恩"},
		{"Pacific/Pohnpei", "Pohnpei", "波纳佩"},
		{"Pacific/Port_Moresby", "Port Moresby", "莫尔兹比港"},
		{"Pacific/Rarotonga", "Rarotonga", "拉罗汤加"},
		{"Pacific/Saipan", "Saipan", "塞班"},
		{"Pacific/Tahiti", "Tahiti", "塔希提"},
		{"Pacific/Tarawa", "Tarawa", "塔拉瓦"},
		{"Pacific/Tongatapu", "Tongatapu", "汤加塔布"},
		{"Pacific/Wake", "Wake", "威克岛"},
		{"Pacific/Wallis", "Wallis", "瓦利斯"},
	}},
	{"Etc (UTC offsets)", "UTC 偏移", []tzCity{
		{"Etc/UTC", "UTC", "UTC"},
		{"Etc/GMT", "GMT", "GMT"},
		{"Etc/GMT+12", "GMT-12", "GMT-12"}, {"Etc/GMT+11", "GMT-11", "GMT-11"},
		{"Etc/GMT+10", "GMT-10", "GMT-10"}, {"Etc/GMT+9", "GMT-9", "GMT-9"},
		{"Etc/GMT+8", "GMT-8", "GMT-8"}, {"Etc/GMT+7", "GMT-7", "GMT-7"},
		{"Etc/GMT+6", "GMT-6", "GMT-6"}, {"Etc/GMT+5", "GMT-5", "GMT-5"},
		{"Etc/GMT+4", "GMT-4", "GMT-4"}, {"Etc/GMT+3", "GMT-3", "GMT-3"},
		{"Etc/GMT+2", "GMT-2", "GMT-2"}, {"Etc/GMT+1", "GMT-1", "GMT-1"},
		{"Etc/GMT-1", "GMT+1", "GMT+1"}, {"Etc/GMT-2", "GMT+2", "GMT+2"},
		{"Etc/GMT-3", "GMT+3", "GMT+3"}, {"Etc/GMT-4", "GMT+4", "GMT+4"},
		{"Etc/GMT-5", "GMT+5", "GMT+5"}, {"Etc/GMT-6", "GMT+6", "GMT+6"},
		{"Etc/GMT-7", "GMT+7", "GMT+7"}, {"Etc/GMT-8", "GMT+8", "GMT+8"},
		{"Etc/GMT-9", "GMT+9", "GMT+9"}, {"Etc/GMT-10", "GMT+10", "GMT+10"},
		{"Etc/GMT-11", "GMT+11", "GMT+11"}, {"Etc/GMT-12", "GMT+12", "GMT+12"},
		{"Etc/GMT-13", "GMT+13", "GMT+13"}, {"Etc/GMT-14", "GMT+14", "GMT+14"},
	}},
}

// 构建 TZ → *tzCity 的索引，便于 O(1) 查表
var tzIndex = func() map[string]*tzCity {
	m := make(map[string]*tzCity, 300)
	for ri := range regions {
		for ci := range regions[ri].cities {
			c := &regions[ri].cities[ci]
			m[c.tz] = c
		}
	}
	return m
}()

func findCityByTZ(tz string) *tzCity { return tzIndex[tz] }

// ============================================================================
// Windows 时区键 → IANA（来自 CLDR windowsZones.xml）
// ============================================================================

var winToIANA = map[string]string{
	"Dateline Standard Time":           "Etc/GMT+12",
	"UTC-11":                           "Etc/GMT+11",
	"Aleutian Standard Time":           "America/Adak",
	"Hawaiian Standard Time":           "Pacific/Honolulu",
	"Marquesas Standard Time":          "Pacific/Marquesas",
	"Alaskan Standard Time":            "America/Anchorage",
	"UTC-09":                           "Etc/GMT+9",
	"Pacific Standard Time (Mexico)":   "America/Tijuana",
	"UTC-08":                           "Etc/GMT+8",
	"Pacific Standard Time":            "America/Los_Angeles",
	"US Mountain Standard Time":        "America/Phoenix",
	"Mountain Standard Time (Mexico)":  "America/Chihuahua",
	"Mountain Standard Time":           "America/Denver",
	"Yukon Standard Time":              "America/Whitehorse",
	"Central America Standard Time":    "America/Guatemala",
	"Central Standard Time":            "America/Chicago",
	"Easter Island Standard Time":      "Pacific/Easter",
	"Central Standard Time (Mexico)":   "America/Mexico_City",
	"Canada Central Standard Time":     "America/Regina",
	"SA Pacific Standard Time":         "America/Bogota",
	"Eastern Standard Time (Mexico)":   "America/Cancun",
	"Eastern Standard Time":            "America/New_York",
	"Haiti Standard Time":              "America/Port-au-Prince",
	"Cuba Standard Time":               "America/Havana",
	"US Eastern Standard Time":         "America/Indiana/Indianapolis",
	"Turks And Caicos Standard Time":   "America/Grand_Turk",
	"Paraguay Standard Time":           "America/Asuncion",
	"Atlantic Standard Time":           "America/Halifax",
	"Venezuela Standard Time":          "America/Caracas",
	"Central Brazilian Standard Time":  "America/Cuiaba",
	"SA Western Standard Time":         "America/La_Paz",
	"Pacific SA Standard Time":         "America/Santiago",
	"Newfoundland Standard Time":       "America/St_Johns",
	"Tocantins Standard Time":          "America/Araguaina",
	"E. South America Standard Time":   "America/Sao_Paulo",
	"SA Eastern Standard Time":         "America/Cayenne",
	"Argentina Standard Time":          "America/Argentina/Buenos_Aires",
	"Greenland Standard Time":          "America/Nuuk",
	"Montevideo Standard Time":         "America/Montevideo",
	"Magallanes Standard Time":         "America/Punta_Arenas",
	"Saint Pierre Standard Time":       "America/Miquelon",
	"Bahia Standard Time":              "America/Bahia",
	"UTC-02":                           "Etc/GMT+2",
	"Azores Standard Time":             "Atlantic/Azores",
	"Cape Verde Standard Time":         "Atlantic/Cape_Verde",
	"UTC":                              "Etc/UTC",
	"GMT Standard Time":                "Europe/London",
	"Greenwich Standard Time":          "Atlantic/Reykjavik",
	"Sao Tome Standard Time":           "Africa/Sao_Tome",
	"Morocco Standard Time":            "Africa/Casablanca",
	"W. Europe Standard Time":          "Europe/Berlin",
	"Central Europe Standard Time":     "Europe/Budapest",
	"Romance Standard Time":            "Europe/Paris",
	"Central European Standard Time":   "Europe/Warsaw",
	"W. Central Africa Standard Time":  "Africa/Lagos",
	"Jordan Standard Time":             "Asia/Amman",
	"GTB Standard Time":                "Europe/Bucharest",
	"Middle East Standard Time":        "Asia/Beirut",
	"Egypt Standard Time":              "Africa/Cairo",
	"E. Europe Standard Time":          "Europe/Chisinau",
	"Syria Standard Time":              "Asia/Damascus",
	"West Bank Standard Time":          "Asia/Hebron",
	"South Africa Standard Time":       "Africa/Johannesburg",
	"FLE Standard Time":                "Europe/Kyiv",
	"Israel Standard Time":             "Asia/Jerusalem",
	"South Sudan Standard Time":        "Africa/Juba",
	"Kaliningrad Standard Time":        "Europe/Kaliningrad",
	"Sudan Standard Time":              "Africa/Khartoum",
	"Libya Standard Time":              "Africa/Tripoli",
	"Namibia Standard Time":            "Africa/Windhoek",
	"Arabic Standard Time":             "Asia/Baghdad",
	"Turkey Standard Time":             "Europe/Istanbul",
	"Arab Standard Time":               "Asia/Riyadh",
	"Belarus Standard Time":            "Europe/Minsk",
	"Russian Standard Time":            "Europe/Moscow",
	"E. Africa Standard Time":          "Africa/Nairobi",
	"Volgograd Standard Time":          "Europe/Volgograd",
	"Iran Standard Time":               "Asia/Tehran",
	"Arabian Standard Time":            "Asia/Dubai",
	"Astrakhan Standard Time":          "Europe/Astrakhan",
	"Azerbaijan Standard Time":         "Asia/Baku",
	"Russia Time Zone 3":               "Europe/Samara",
	"Mauritius Standard Time":          "Indian/Mauritius",
	"Saratov Standard Time":            "Europe/Saratov",
	"Georgian Standard Time":           "Asia/Tbilisi",
	"Caucasus Standard Time":           "Asia/Yerevan",
	"Afghanistan Standard Time":        "Asia/Kabul",
	"West Asia Standard Time":          "Asia/Tashkent",
	"Ekaterinburg Standard Time":       "Asia/Yekaterinburg",
	"Pakistan Standard Time":           "Asia/Karachi",
	"Qyzylorda Standard Time":          "Asia/Qyzylorda",
	"India Standard Time":              "Asia/Kolkata",
	"Sri Lanka Standard Time":          "Asia/Colombo",
	"Nepal Standard Time":              "Asia/Kathmandu",
	"Central Asia Standard Time":       "Asia/Almaty",
	"Bangladesh Standard Time":         "Asia/Dhaka",
	"Omsk Standard Time":               "Asia/Omsk",
	"Myanmar Standard Time":            "Asia/Yangon",
	"SE Asia Standard Time":            "Asia/Bangkok",
	"Altai Standard Time":              "Asia/Barnaul",
	"W. Mongolia Standard Time":        "Asia/Hovd",
	"North Asia Standard Time":         "Asia/Krasnoyarsk",
	"N. Central Asia Standard Time":    "Asia/Novosibirsk",
	"Tomsk Standard Time":              "Asia/Tomsk",
	"China Standard Time":              "Asia/Shanghai",
	"North Asia East Standard Time":    "Asia/Irkutsk",
	"Singapore Standard Time":          "Asia/Singapore",
	"W. Australia Standard Time":       "Australia/Perth",
	"Taipei Standard Time":             "Asia/Taipei",
	"Ulaanbaatar Standard Time":        "Asia/Ulaanbaatar",
	"Aus Central W. Standard Time":     "Australia/Eucla",
	"Transbaikal Standard Time":        "Asia/Chita",
	"Tokyo Standard Time":              "Asia/Tokyo",
	"North Korea Standard Time":        "Asia/Pyongyang",
	"Korea Standard Time":              "Asia/Seoul",
	"Yakutsk Standard Time":            "Asia/Yakutsk",
	"Cen. Australia Standard Time":     "Australia/Adelaide",
	"AUS Central Standard Time":        "Australia/Darwin",
	"E. Australia Standard Time":       "Australia/Brisbane",
	"AUS Eastern Standard Time":        "Australia/Sydney",
	"West Pacific Standard Time":       "Pacific/Port_Moresby",
	"Tasmania Standard Time":           "Australia/Hobart",
	"Vladivostok Standard Time":        "Asia/Vladivostok",
	"Lord Howe Standard Time":          "Australia/Lord_Howe",
	"Bougainville Standard Time":       "Pacific/Bougainville",
	"Russia Time Zone 10":              "Asia/Srednekolymsk",
	"Magadan Standard Time":            "Asia/Magadan",
	"Norfolk Standard Time":            "Pacific/Norfolk",
	"Sakhalin Standard Time":           "Asia/Sakhalin",
	"Central Pacific Standard Time":    "Pacific/Guadalcanal",
	"Russia Time Zone 11":              "Asia/Kamchatka",
	"New Zealand Standard Time":        "Pacific/Auckland",
	"UTC+12":                           "Etc/GMT-12",
	"Fiji Standard Time":               "Pacific/Fiji",
	"Kamchatka Standard Time":          "Asia/Kamchatka",
	"Chatham Islands Standard Time":    "Pacific/Chatham",
	"UTC+13":                           "Etc/GMT-13",
	"Tonga Standard Time":              "Pacific/Tongatapu",
	"Samoa Standard Time":              "Pacific/Apia",
	"Line Islands Standard Time":       "Pacific/Kiritimati",
}

// detectLocalIANA 从注册表拿 Windows 时区键，再映射到 IANA。
func detectLocalIANA() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\TimeZoneInformation`,
		registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	winKey, _, err := k.GetStringValue("TimeZoneKeyName")
	if err != nil || winKey == "" {
		return ""
	}
	winKey = strings.TrimRight(winKey, "\x00 ")
	if iana, ok := winToIANA[winKey]; ok {
		return iana
	}
	return ""
}

// ============================================================================
// 状态与持久化
// ============================================================================

type zoneEntry struct {
	Label string `json:"label"`
	TZ    string `json:"tz"`
}

type appState struct {
	Zones       []zoneEntry `json:"zones"`
	FontSize    int32       `json:"font_size"`
	ShowSeconds bool        `json:"show_seconds"`
	Dark        bool        `json:"dark"`
	Opacity     int32       `json:"opacity"` // 50..255
	WinX        int32       `json:"x"`
	WinY        int32       `json:"y"`
	Lang        string      `json:"lang"` // "cn" | "en"
}

func isEN() bool { return state.Lang == "en" }

func tr(cn, en string) string {
	if isEN() {
		return en
	}
	return cn
}
func cityLabel(c tzCity) string {
	if isEN() {
		return c.en
	}
	return c.cn
}
func regionLabel(r tzRegion) string {
	if isEN() {
		return r.en
	}
	return r.cn
}

// offsetLabel 没在 CLDR 映射表里时的兜底：UTC±HH[:MM]
func offsetLabel() string {
	_, off := time.Now().Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	h, m := off/3600, (off%3600)/60
	if m == 0 {
		return fmt.Sprintf("UTC%s%d", sign, h)
	}
	return fmt.Sprintf("UTC%s%d:%02d", sign, h, m)
}

// localEntry 生成本机时区条目。优先用 CLDR 映射查到的城市名；失败则 UTC 偏移兜底。
func localEntry() zoneEntry {
	iana := detectLocalIANA()
	if iana != "" {
		if c := findCityByTZ(iana); c != nil {
			return zoneEntry{Label: cityLabel(*c), TZ: iana}
		}
		return zoneEntry{Label: iana, TZ: iana}
	}
	return zoneEntry{Label: offsetLabel(), TZ: "Local"}
}

// refreshLabels 按当前语言把 state.Zones 里所有能识别的条目的 label 刷新一遍。
// 这样切换语言 / 启动时加载旧 state / 迁移 Label 格式都自动生效。
func refreshLabels() {
	for i, z := range state.Zones {
		if z.TZ == "Local" {
			state.Zones[i].Label = offsetLabel()
			continue
		}
		if c := findCityByTZ(z.TZ); c != nil {
			state.Zones[i].Label = cityLabel(*c)
		}
	}
}

func defaultState() appState {
	return appState{
		Zones:       nil, // 延后用 localEntry 填
		FontSize:    16,
		ShowSeconds: true,
		Dark:        true,
		Opacity:     235,
		WinX:        200,
		WinY:        200,
		Lang:        "cn",
	}
}

func loadState() appState {
	s := defaultState()
	k, err := registry.OpenKey(registry.CURRENT_USER, regSubKey, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if raw, _, err := k.GetStringValue("state"); err == nil && raw != "" {
			var parsed appState
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				s = parsed
			}
		}
	}
	if s.Lang != "en" && s.Lang != "cn" {
		s.Lang = "cn"
	}
	if s.FontSize <= 0 {
		s.FontSize = 16
	}
	if s.Opacity < 50 || s.Opacity > 255 {
		s.Opacity = 235
	}
	if len(s.Zones) == 0 {
		s.Zones = []zoneEntry{localEntry()}
	}
	// 拿 s 的 Lang 刷一次 label —— 这时候全局 state 还是默认值，所以借用临时切换
	state = s
	refreshLabels()
	return state
}

func saveState() {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, regSubKey, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = k.SetStringValue("state", string(b))
}

// ============================================================================
// 联网校时
// ============================================================================

var netOffsetNS atomic.Int64

func correctedNow() time.Time { return time.Now().Add(time.Duration(netOffsetNS.Load())) }

func fetchUTC(ctx context.Context, cli *http.Client) (time.Time, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://worldtimeapi.org/api/timezone/Etc/UTC", nil)
	if resp, err := cli.Do(req); err == nil {
		var r struct {
			UTCDatetime string `json:"utc_datetime"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if derr == nil {
			if t, perr := time.Parse(time.RFC3339Nano, r.UTCDatetime); perr == nil {
				return t, nil
			}
		}
	}
	req2, _ := http.NewRequestWithContext(ctx, "HEAD", "https://www.google.com/generate_204", nil)
	if resp, err := cli.Do(req2); err == nil {
		d := resp.Header.Get("Date")
		resp.Body.Close()
		if d != "" {
			if t, perr := http.ParseTime(d); perr == nil {
				return t.UTC(), nil
			}
		}
	}
	return time.Time{}, errors.New("all time sources failed")
}

func syncNet(ctx context.Context) {
	cli := &http.Client{Timeout: 3 * time.Second}
	try := func() {
		if t, err := fetchUTC(ctx, cli); err == nil {
			netOffsetNS.Store(int64(t.Sub(time.Now().UTC())))
		}
	}
	try()
	tk := time.NewTicker(10 * time.Minute)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			try()
		}
	}
}

// ============================================================================
// 全局 UI 状态
// ============================================================================

var (
	hwnd    windows.HWND
	hfont   uintptr
	bkBrush uintptr

	// state 只在 UI 线程访问（runtime.LockOSThread 锁在 main 里）。
	// 唯一的后台 goroutine syncNet 只写 netOffsetNS 原子字段，不碰 state。
	// 所以这里不需要 mutex。
	state appState

	menuCBs   = map[uint32]func(){}
	nextCmdID uint32 = 2000
)

func resetMenu() { menuCBs = map[uint32]func(){}; nextCmdID = 2000 }
func registerCmd(cb func()) uint32 {
	id := nextCmdID
	nextCmdID++
	menuCBs[id] = cb
	return id
}

// ============================================================================
// GDI 资源
// ============================================================================

func ensureFont() {
	r, _, _ := procCreateFontW.Call(
		uintptr(-int32(state.FontSize)), 0, 0, 0, fw_NORMAL,
		0, 0, 0, defaultCharset, outDefaultPrecis, clipDefaultPrecis,
		cleartypeQuality, defaultPitch,
		uintptr(unsafe.Pointer(u16("Microsoft YaHei"))),
	)
	if r == 0 {
		return // 保留旧的 hfont，这种情况极罕见（资源耗尽）
	}
	if hfont != 0 {
		procDeleteObject.Call(hfont)
	}
	hfont = r
}

func ensureBrush() {
	if bkBrush != 0 {
		procDeleteObject.Call(bkBrush)
		bkBrush = 0
	}
	var col uintptr
	if state.Dark {
		col = rgb(22, 24, 30)
	} else {
		col = rgb(248, 248, 250)
	}
	r, _, _ := procCreateSolidBrush.Call(col)
	bkBrush = r
}

func textColor() uintptr {
	if state.Dark {
		return rgb(232, 232, 238)
	}
	return rgb(28, 28, 32)
}

// ============================================================================
// 渲染
// ============================================================================

const (
	padX = 14
	padY = 10
	gapX = 18
	gapY = 6
)

func formatTime(z zoneEntry) string {
	loc := time.Local
	if z.TZ != "" && z.TZ != "Local" {
		if l, err := time.LoadLocation(z.TZ); err == nil {
			loc = l
		}
	}
	layout := "15:04"
	if state.ShowSeconds {
		layout = "15:04:05"
	}
	return correctedNow().In(loc).Format(layout)
}

func measure(hdc uintptr, s string) int32 {
	p := u16slice(s)
	if len(p) == 0 {
		return 0
	}
	var sz sizeStruct
	procGetTextExtentPoint32W.Call(hdc,
		uintptr(unsafe.Pointer(&p[0])),
		uintptr(len(p)),
		uintptr(unsafe.Pointer(&sz)))
	return sz.CX
}

func drawText(hdc uintptr, x, y int32, s string) {
	p := u16slice(s)
	if len(p) == 0 {
		return
	}
	procTextOutW.Call(hdc, uintptr(x), uintptr(y),
		uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
}

// measureContent 用 hwnd 自己的 DC 实测：label 列最宽、time 列最宽、单行高度。
// 无论是 desiredSize 还是 paint 都走这条路径，保证二者永远同步。
func measureContent() (labelW, timeW, lineH int32) {
	if hwnd == 0 {
		// 窗口还没建好时的兜底估算
		lineH = state.FontSize + 8
		labelW = state.FontSize * 4
		timeW = state.FontSize * 4
		if state.ShowSeconds {
			timeW = state.FontSize * 5
		}
		return
	}
	hdc, _, _ := procGetDC.Call(uintptr(hwnd))
	defer procReleaseDC.Call(uintptr(hwnd), hdc)
	procSelectObject.Call(hdc, hfont)

	for _, z := range state.Zones {
		var sz sizeStruct
		if p := u16slice(z.Label); len(p) > 0 {
			procGetTextExtentPoint32W.Call(hdc,
				uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)),
				uintptr(unsafe.Pointer(&sz)))
			if sz.CX > labelW {
				labelW = sz.CX
			}
			if sz.CY > lineH {
				lineH = sz.CY
			}
		}
		if tp := u16slice(formatTime(z)); len(tp) > 0 {
			procGetTextExtentPoint32W.Call(hdc,
				uintptr(unsafe.Pointer(&tp[0])), uintptr(len(tp)),
				uintptr(unsafe.Pointer(&sz)))
			if sz.CX > timeW {
				timeW = sz.CX
			}
		}
	}
	if lineH == 0 {
		lineH = state.FontSize + 4
	}
	lineH += int32(gapY)
	return
}

func paint(h windows.HWND) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))

	var cr rect
	procGetClientRect.Call(uintptr(h), uintptr(unsafe.Pointer(&cr)))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&cr)), bkBrush)

	procSelectObject.Call(hdc, hfont)
	procSetBkMode.Call(hdc, bkModeTransparent)
	procSetTextColor.Call(hdc, textColor())

	zones := append([]zoneEntry(nil), state.Zones...)

	labelW, _, lineH := measureContent()
	timeX := int32(padX) + labelW + int32(gapX)

	y := int32(padY)
	for _, z := range zones {
		drawText(hdc, int32(padX), y, z.Label)
		drawText(hdc, timeX, y, formatTime(z))
		y += lineH
	}
}

// ============================================================================
// 窗口尺寸
// ============================================================================

func desiredSize() (w, h int32) {
	labelW, timeW, lineH := measureContent()
	w = int32(padX)*2 + labelW + int32(gapX) + timeW
	n := int32(len(state.Zones))
	if n < 1 {
		n = 1
	}
	h = int32(padY)*2 + n*lineH
	// 最低安全宽度：防止 0 zone 或测量失败时窗口塌缩
	if w < 96 {
		w = 96
	}
	return
}

func resizeWindow() {
	w, h := desiredSize()
	procSetWindowPos.Call(uintptr(hwnd), 0,
		uintptr(state.WinX), uintptr(state.WinY),
		uintptr(w), uintptr(h),
		swp_NOZORDER|swp_NOACTIVATE)
	procInvalidateRect.Call(uintptr(hwnd), 0, 1)
}

func applyOpacity() {
	procSetLayeredWindowAttrs.Call(uintptr(hwnd), 0, uintptr(state.Opacity), lwa_ALPHA)
}

// ============================================================================
// 菜单
// ============================================================================

func newPopupMenu() uintptr { r, _, _ := procCreatePopupMenu.Call(); return r }
func appendStr(menu uintptr, id uint32, label string, flags uintptr) {
	procAppendMenuW.Call(menu, mf_STRING|flags, uintptr(id), uintptr(unsafe.Pointer(u16(label))))
}
func appendSub(menu, sub uintptr, label string, flags uintptr) {
	procAppendMenuW.Call(menu, mf_POPUP|flags, sub, uintptr(unsafe.Pointer(u16(label))))
}
func appendSep(menu uintptr) { procAppendMenuW.Call(menu, mf_SEPARATOR, 0, 0) }

// checkedFlag：on 时返回 MF_CHECKED，让 Windows 自己画原生对勾，避免 "✓" 字符和空格列宽不等。
func checkedFlag(on bool) uintptr {
	if on {
		return mf_CHECKED
	}
	return 0
}

func buildMenu() uintptr {
	resetMenu()
	root := newPopupMenu()

	// ---- 添加时区 ----
	addRoot := newPopupMenu()
	full := len(state.Zones) >= maxZones
	for ri := range regions {
		reg := regions[ri]
		sub := newPopupMenu()
		for ci := range reg.cities {
			city := reg.cities[ci]
			exists := false
			for _, z := range state.Zones {
				if z.TZ == city.tz {
					exists = true
					break
				}
			}
			id := registerCmd(func() {
				if len(state.Zones) >= maxZones {
					return
				}
				for _, z := range state.Zones {
					if z.TZ == city.tz {
						return
					}
				}
				state.Zones = append(state.Zones, zoneEntry{Label: cityLabel(city), TZ: city.tz})
				saveState()
				resizeWindow()
			})
			var fl uintptr
			if exists || full {
				fl = mf_GRAYED
			}
			appendStr(sub, id, cityLabel(city), fl)
		}
		appendSub(addRoot, sub, regionLabel(reg), 0)
	}
	addLabel := tr("添加时区", "Add Zone")
	if full {
		addLabel = tr("添加时区（已达上限 10）", "Add Zone (max 10 reached)")
		appendSub(root, addRoot, addLabel, mf_GRAYED)
	} else {
		appendSub(root, addRoot, addLabel, 0)
	}

	// ---- 删除时区 ----
	delSub := newPopupMenu()
	for i, z := range state.Zones {
		idx := i
		zz := z
		id := registerCmd(func() {
			if idx < 0 || idx >= len(state.Zones) || len(state.Zones) <= 1 {
				return
			}
			state.Zones = append(state.Zones[:idx], state.Zones[idx+1:]...)
			saveState()
			resizeWindow()
		})
		appendStr(delSub, id, zz.Label+"   ("+zz.TZ+")", 0)
	}
	appendSub(root, delSub, tr("删除时区", "Remove Zone"), 0)

	appendSep(root)

	// ---- 显示秒 ----
	appendStr(root, registerCmd(func() {
		state.ShowSeconds = !state.ShowSeconds
		saveState()
		resizeWindow()
	}), tr("显示秒", "Show seconds"), checkedFlag(state.ShowSeconds))

	// ---- 字号 ----
	sizeSub := newPopupMenu()
	for _, p := range []struct {
		label string
		val   int32
	}{
		{tr("小 (13)", "Small (13)"), 13},
		{tr("中 (16)", "Medium (16)"), 16},
		{tr("大 (20)", "Large (20)"), 20},
		{tr("特大 (24)", "X-Large (24)"), 24},
	} {
		pp := p
		id := registerCmd(func() {
			state.FontSize = pp.val
			saveState()
			ensureFont()
			resizeWindow()
		})
		appendStr(sizeSub, id, pp.label, checkedFlag(state.FontSize == pp.val))
	}
	appendSub(root, sizeSub, tr("字号", "Font size"), 0)

	// ---- 透明度 ----
	opSub := newPopupMenu()
	for _, p := range []struct {
		label string
		val   int32
	}{{"100%", 255}, {"90%", 230}, {"80%", 205}, {"70%", 180}, {"60%", 155}} {
		pp := p
		id := registerCmd(func() {
			state.Opacity = pp.val
			saveState()
			applyOpacity()
		})
		appendStr(opSub, id, pp.label, checkedFlag(state.Opacity == pp.val))
	}
	appendSub(root, opSub, tr("透明度", "Opacity"), 0)

	// ---- 主题 ----
	themeSub := newPopupMenu()
	appendStr(themeSub, registerCmd(func() {
		state.Dark = true
		saveState()
		ensureBrush()
		procInvalidateRect.Call(uintptr(hwnd), 0, 1)
	}), tr("深色", "Dark"), checkedFlag(state.Dark))
	appendStr(themeSub, registerCmd(func() {
		state.Dark = false
		saveState()
		ensureBrush()
		procInvalidateRect.Call(uintptr(hwnd), 0, 1)
	}), tr("浅色", "Light"), checkedFlag(!state.Dark))
	appendSub(root, themeSub, tr("主题", "Theme"), 0)

	// ---- 语言 ----
	langSub := newPopupMenu()
	appendStr(langSub, registerCmd(func() {
		state.Lang = "cn"
		refreshLabels()
		saveState()
		resizeWindow()
	}), "中文", checkedFlag(state.Lang == "cn"))
	appendStr(langSub, registerCmd(func() {
		state.Lang = "en"
		refreshLabels()
		saveState()
		resizeWindow()
	}), "English", checkedFlag(state.Lang == "en"))
	appendSub(root, langSub, tr("语言", "Language"), 0)

	appendSep(root)

	// ---- 恢复默认 ----
	appendStr(root, registerCmd(func() {
		keepLang := state.Lang
		state = defaultState()
		state.Lang = keepLang
		state.Zones = []zoneEntry{localEntry()}
		refreshLabels()
		saveState()
		ensureBrush()
		ensureFont()
		applyOpacity()
		resizeWindow()
	}), tr("恢复默认", "Reset to defaults"), 0)

	// ---- 退出 ----
	appendStr(root, registerCmd(func() {
		saveWindowPos()
		saveState()
		procPostQuitMessage.Call(0)
	}), tr("退出", "Quit"), 0)

	return root
}

func showMenu() {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	menu := buildMenu()
	defer procDestroyMenu.Call(menu)
	r, _, _ := procTrackPopupMenu.Call(menu,
		tpm_LEFTALIGN|tpm_RIGHTBUTTON|tpm_RETURNCMD,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	if r != 0 {
		if cb := menuCBs[uint32(r)]; cb != nil {
			cb()
		}
	}
}

// ============================================================================
// 窗口过程 & main
// ============================================================================

func saveWindowPos() {
	var rc rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
	state.WinX = rc.Left
	state.WinY = rc.Top
	saveState()
}

func wndProc(h windows.HWND, msg uint32, wp, lp uintptr) uintptr {
	switch msg {
	case wmERASEBKGND:
		return 1
	case wmPAINT:
		paint(h)
		return 0
	case wmTIMER:
		procInvalidateRect.Call(uintptr(h), 0, 0)
		return 0
	case wmLBUTTONDOWN:
		procReleaseCapture.Call()
		procSendMessageW.Call(uintptr(h), wmNCLBUTTONDOWN, htCAPTION, 0)
		return 0
	case wmRBUTTONUP:
		showMenu()
		return 0
	case wmEXITSIZEMOVE:
		saveWindowPos()
		return 0
	case wmSETCURSOR:
		cur, _, _ := procLoadCursorW.Call(0, idcARROW)
		procSetCursor.Call(cur)
		return 1
	case wmCLOSE:
		saveWindowPos()
		saveState()
		procPostQuitMessage.Call(0)
		return 0
	case wmDESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(msg), wp, lp)
	return r
}

func main() {
	// 关键：Windows 消息循环 + syscall.NewCallback 必须钉在同一个 OS 线程上，
	// 否则 Go 调度器迁移 goroutine 时，回调被别的线程调用会挂死。
	runtime.LockOSThread()

	procSetProcessDPIAware.Call()

	state = loadState()

	ensureBrush()
	ensureFont()

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	hCursor, _, _ := procLoadCursorW.Call(0, idcARROW)

	classNameP := u16(appClassName)
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		Style:     cs_HREDRAW | cs_VREDRAW,
		WndProc:   syscall.NewCallback(wndProc),
		Instance:  windows.Handle(hInstance),
		Cursor:    windows.Handle(hCursor),
		ClassName: classNameP,
	}
	if ret, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		fatal(fmt.Sprintf("RegisterClassEx failed: %v", err))
	}

	hw, _, err := procCreateWindowEx.Call(
		wsEx_TOPMOST|wsEx_LAYERED|wsEx_TOOLWINDOW,
		uintptr(unsafe.Pointer(classNameP)),
		uintptr(unsafe.Pointer(u16("DesktopTime"))),
		ws_POPUP|ws_VISIBLE,
		uintptr(state.WinX), uintptr(state.WinY),
		280, 100,
		0, 0, hInstance, 0,
	)
	if hw == 0 {
		fatal(fmt.Sprintf("CreateWindowEx failed: %v", err))
	}
	hwnd = windows.HWND(hw)
	applyOpacity()
	resizeWindow()
	procShowWindow.Call(uintptr(hwnd), sw_SHOWNOACTIVATE)
	procUpdateWindow.Call(uintptr(hwnd))

	procSetTimer.Call(uintptr(hwnd), timerRefresh, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go syncNet(ctx)

	var m msgT
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}
