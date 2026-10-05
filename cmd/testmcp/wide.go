package main

// Wide catalogue mode (RELAY_TESTMCP_CATALOG=wide): 48 tools, one per domain,
// big enough that their definitions alone cost a chat turn tens of thousands
// of bytes. The chat-tool-search-tokens journey measures that cost with and
// without tool search. Off by default, so every other test sees the one-tool
// peer it always saw.
//
// `testmcp --write-skills DIR` writes the matching skills (one per domain,
// each listing its one tool) under DIR/.claude/skills, the layout relay's
// chat tool search reads.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func wideMode() bool { return os.Getenv("RELAY_TESTMCP_CATALOG") == "wide" }

// wideDomain: the tool is <domain>_<verb>; asks completes "Use when the user
// asks for ..."; keywords feed the skill's ranking text.
type wideDomain struct{ domain, verb, summary, asks, keywords string }

var wideDomains = []wideDomain{
	{"tides", "lookup", "Look up the tide code and the high and low tide schedule for a harbour", "tide times, the tide code or the water level for a named port", "tide, harbour, port, sea, water level"},
	{"weather", "forecast", "Fetch the weather forecast for a town", "rain, wind, temperature or a multi-day outlook for a place", "weather, forecast, rain, wind, temperature"},
	{"flights", "search", "Search scheduled flights between two airports", "flight options, fares or departure times between two cities", "flight, airline, airport, fare, departure"},
	{"hotels", "search", "Search hotels with rates and availability", "a room, a hotel stay or nightly rates in a city", "hotel, room, stay, booking, lodging"},
	{"trains", "timetable", "Read the rail timetable for a route", "train times, platforms or rail connections between two stations", "train, rail, station, platform, timetable"},
	{"buses", "routes", "List local bus routes and stops", "bus lines, stops or the next bus near an address", "bus, stop, route, transit, commute"},
	{"taxis", "estimate", "Estimate a taxi fare and wait time", "a cab fare, a ride estimate or a pickup time", "taxi, cab, ride, fare, pickup"},
	{"ferries", "schedule", "Read ferry crossings and sailing times", "ferry sailings, crossing times or vehicle deck space", "ferry, sailing, crossing, vessel, deck"},
	{"recipes", "find", "Find recipes by ingredient or dish", "a dinner idea, a recipe or a way to cook an ingredient", "recipe, cooking, dish, ingredient, dinner"},
	{"groceries", "list", "Manage a shopping list for a supermarket", "a grocery list, items to buy or store aisles", "grocery, shopping, supermarket, list, aisle"},
	{"pharmacy", "refill", "Check prescription refills at a pharmacy", "a prescription refill, pickup status or dosage reminders", "pharmacy, prescription, refill, medicine, dose"},
	{"fitness", "log", "Record a workout in the fitness log", "a workout entry, exercise totals or training progress", "fitness, workout, exercise, training, gym"},
	{"sleep", "report", "Report sleep duration and quality", "last night's sleep, bedtime habits or a sleep score", "sleep, bedtime, rest, insomnia, score"},
	{"meditation", "session", "Start a guided meditation session", "a breathing exercise, a calm minute or a guided session", "meditation, breathing, calm, mindfulness, relax"},
	{"calendar", "agenda", "Read the calendar agenda for a day", "today's meetings, free slots or an agenda for a date", "calendar, agenda, meeting, schedule, event"},
	{"contacts", "lookup", "Look up a contact card", "a phone number, an email address or a birthday for a person", "contact, phone, address book, person, birthday"},
	{"notes", "search", "Search personal notes", "an old note, a saved snippet or a memo about a topic", "note, memo, snippet, journal, jot"},
	{"reminders", "create", "Create a timed reminder", "a reminder, a nudge at a given time or a repeating alert", "reminder, alert, nudge, timer, remember"},
	{"invoices", "status", "Check the status of a customer invoice", "an invoice, payment status or an overdue bill", "invoice, bill, payment, overdue, customer"},
	{"expenses", "report", "Summarise expenses for a period", "spending totals, expense claims or a receipt breakdown", "expense, spending, receipt, claim, budget"},
	{"payroll", "summary", "Summarise payroll for a pay period", "a payslip, pay dates or salary deductions", "payroll, payslip, salary, wages, deduction"},
	{"taxes", "estimate", "Estimate tax owed for a year", "a tax estimate, a deadline or a filing status", "tax, filing, deadline, return, deduction"},
	{"stocks", "quote", "Quote a stock price and daily range", "a share price, a ticker quote or a market move", "stock, share, ticker, market, quote"},
	{"crypto", "price", "Quote a cryptocurrency price", "a coin price, a token quote or a crypto market move", "crypto, coin, token, bitcoin, wallet"},
	{"savings", "projection", "Project growth of a savings balance", "interest earned, a savings goal or a balance projection", "savings, interest, goal, balance, deposit"},
	{"loans", "compare", "Compare loan offers by rate and term", "a loan rate, a repayment plan or an offer comparison", "loan, rate, repayment, borrow, term"},
	{"insurance", "quote", "Quote an insurance premium", "a premium, a cover level or a policy comparison", "insurance, premium, policy, cover, claim"},
	{"mortgage", "calculate", "Calculate a mortgage payment", "a monthly mortgage payment, a deposit or an affordability check", "mortgage, home loan, deposit, payment, property"},
	{"music", "play", "Play music by artist or mood", "a song, an album, a playlist or music for a mood", "music, song, album, playlist, artist"},
	{"podcasts", "episodes", "List podcast episodes", "the latest podcast episodes, a show or a download", "podcast, episode, show, audio, subscribe"},
	{"movies", "showtimes", "Read cinema showtimes", "a film, cinema showtimes or what is on tonight", "movie, film, cinema, showtime, ticket"},
	{"books", "recommend", "Recommend books by genre", "a book to read, a novel or a reading list", "book, novel, reading, author, genre"},
	{"games", "scores", "Report video game scores and ladders", "a game score, a ladder rank or a match result", "game, score, ladder, match, player"},
	{"news", "headlines", "Fetch the news headlines", "today's headlines, a news summary or a story on a topic", "news, headline, story, press, update"},
	{"sports", "results", "Report sports results and fixtures", "a match result, a league table or the next fixture", "sports, result, league, fixture, team"},
	{"trivia", "question", "Ask a trivia question", "a quiz question, a fun fact or a trivia round", "trivia, quiz, fact, question, puzzle"},
	{"translate", "text", "Translate text between languages", "a translation, a phrase in another language or a language check", "translate, language, phrase, foreign, interpreter"},
	{"spelling", "check", "Check spelling and grammar", "a spelling check, a grammar fix or a proofread", "spelling, grammar, proofread, typo, edit"},
	{"thesaurus", "synonyms", "List synonyms and antonyms", "a synonym, an antonym or a better word", "synonym, antonym, word, thesaurus, vocabulary"},
	{"units", "convert", "Convert between units of measure", "a unit conversion, miles to kilometres or cups to grams", "unit, convert, metric, imperial, measure"},
	{"currency", "convert", "Convert between currencies at the day's rate", "a currency conversion, an exchange rate or a travel budget", "currency, exchange, rate, money, foreign"},
	{"timezone", "convert", "Convert a time between zones", "the time in another city, a meeting across zones or a UTC offset", "timezone, clock, utc, offset, world time"},
	{"maps", "route", "Plan a route between two places", "directions, a route, a travel time or a distance", "map, route, directions, distance, navigate"},
	{"parking", "find", "Find parking near a destination", "a parking spot, a car park or parking rates", "parking, car park, garage, rates, space"},
	{"plants", "care", "Give care advice for a houseplant", "how to water a plant, light needs or a plant disease", "plant, houseplant, water, light, leaf"},
	{"pets", "vet", "Book and track vet visits for a pet", "a vet appointment, a vaccination or a pet record", "pet, vet, dog, cat, vaccination"},
	{"garden", "planner", "Plan a garden bed by season", "what to sow now, a planting plan or a garden layout", "garden, sow, season, soil, bed"},
	{"recycling", "rules", "Look up local recycling rules", "which bin an item goes in, collection days or recycling rules", "recycling, bin, waste, collection, compost"},
}

func (d wideDomain) tool() string { return d.domain + "_" + d.verb }

func (d wideDomain) description() string {
	return fmt.Sprintf("%s. Use when the user asks for %s.\n\n"+
		"When to use: pick this tool for one concrete request about %s, such as a single lookup, a short comparison or a quick check. Name the subject in the query exactly as the user phrased it. If the request mixes several topics, call the matching tool for each topic separately rather than packing them into one query.\n\n"+
		"When not to use: do not use it to change an account, make a booking, send a message or pay for anything. It only reads and reports. Do not use it for general knowledge questions that need no live data, and do not call it again with the same arguments within one turn; reuse the earlier answer.\n\n"+
		"Output: a short plain-text answer for one request, with the figures or names the user asked about first and any caveat after them. When the %s service has no answer, the text says what is missing and which argument to change. It never returns markup, links that need a login, or personal data about other people.\n\n"+
		"Limits: at most 20 calls a minute per session. Answers are cached for five minutes. Times use the region's local clock unless the answer states a zone. Related terms the service understands: %s.",
		d.summary, d.asks, d.asks, d.domain, d.keywords)
}

// schema gives the tool its input schema. tides_lookup is pinned by the
// journey's contract: a required port and an optional date, nothing else.
// Every other tool gets 6-8 described properties, sized so the whole
// catalogue's definitions are well over 130 KB as JSON: the chat-tool-search
// journey needs auto mode to fire on a 262144-token window (10% = 26214
// tokens, about 105 KB at 4 bytes a token).
func (d wideDomain) schema(i int) map[string]any {
	if d.tool() == "tides_lookup" {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{"type": "string", "description": "The name of the port or harbour to look up, for example the town on the chart. Letter case and surrounding spaces do not matter."},
				"date": map[string]any{"type": "string", "description": "The day to look up, as YYYY-MM-DD. Leave it out for today."},
			},
			"required": []string{"port"},
		}
	}
	about := d.asks
	props := map[string]any{
		"query":    map[string]any{"type": "string", "description": "What to look for in this domain, in a few words. Name the place, person, item or topic the request is about, as the user phrased it, so the answer can be matched to " + about + ". Do not add instructions or politeness; keep it to the subject. Example subjects for " + d.domain + " are: " + d.keywords + "."},
		"region":   map[string]any{"type": "string", "description": "Where the request applies: a town, a country or a region code. Leave it out to use the account's home region, which is right for most requests about " + d.domain + ". A region the service does not cover is answered with the nearest covered one, and the answer says so."},
		"date":     map[string]any{"type": "string", "description": "The day the request is about, as YYYY-MM-DD. Leave it out for today. A date in the past returns the recorded " + d.domain + " answer for that day; a date more than a year ahead is refused with a message that names the latest accepted day."},
		"limit":    map[string]any{"type": "integer", "description": "The most results to return, from 1 to 50. The default is 10. Use a small number when the user wants one answer about " + d.domain + " rather than a list, and a larger one only when the user asks to see every option."},
		"language": map[string]any{"type": "string", "description": "The language of the answer as an IETF tag such as en or de. The default is the language of the user's request. Names of places and people stay in their local spelling whatever this is set to, so a " + d.domain + " answer never renames the subject."},
		"units":    map[string]any{"type": "string", "enum": []string{"metric", "imperial"}, "description": "The unit system for any distance, weight, volume or temperature in the answer. The default follows the region. Set it only when the user states a preference, because a wrong guess makes the " + d.domain + " figures hard to compare with what the user already knows."},
	}
	if i%2 == 0 {
		props["detail"] = map[string]any{"type": "string", "enum": []string{"brief", "full"}, "description": "How much to say: brief or full. The default is brief. Choose full only when the user asks for every detail about " + d.domain + " in the answer, because the full text is several times longer and costs the user reading time."}
	}
	if i%3 == 0 {
		props["sort"] = map[string]any{"type": "string", "enum": []string{"relevance", "newest", "nearest"}, "description": "The order of the results: relevance, newest or nearest. The default is relevance. Use nearest only when the region argument names a place, and newest only when the user cares about recent " + d.domain + " information over the best match."}
	}
	if i%4 == 0 {
		props["include_sources"] = map[string]any{"type": "boolean", "description": "Whether to name where each " + d.domain + " figure came from. The default is false. Set it to true when the user asks how sure the answer is or where the information was taken from."}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"query"}}
}

func wideToolList() []map[string]any {
	tools := make([]map[string]any, 0, len(wideDomains))
	for i, d := range wideDomains {
		tools = append(tools, map[string]any{
			"name":        d.tool(),
			"description": d.description(),
			"inputSchema": d.schema(i),
			"category":    d.domain,
		})
	}
	return tools
}

// tideCode is the answer tides_lookup gives for a port, so a journey can
// compute the expected value itself.
func tideCode(port string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(port))))
	return "TIDE-" + hex.EncodeToString(sum[:])[:8]
}

// wideCall answers a tools/call in wide mode, as an MCP CallToolResult.
func wideCall(params json.RawMessage) json.RawMessage {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	text, isErr := "ok "+p.Name, false
	if p.Name == "tides_lookup" {
		var a struct {
			Port string `json:"port"`
		}
		if json.Unmarshal(p.Arguments, &a) != nil || strings.TrimSpace(a.Port) == "" {
			text, isErr = "port is required", true
		} else {
			text = tideCode(a.Port)
		}
	}
	b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr})
	return b
}

// writeWideSkills writes one skill per domain under dir/.claude/skills, in
// the shape relay's renderBucketSkillMd produces, heading count included
// ("## Tools (N)").
func writeWideSkills(dir string) error {
	for _, d := range wideDomains {
		skillDir := filepath.Join(dir, ".claude", "skills", "relay-"+d.domain)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return err
		}
		doc := fmt.Sprintf("---\nname: relay-%s\ndescription: %s\nkeywords: [%s]\n---\n\n# %s\n\n## Tools (1)\n\n- **%s** — %s\n",
			d.domain, d.summary, d.keywords, d.domain, d.tool(), d.summary)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(doc), 0o644); err != nil {
			return err
		}
	}
	return nil
}
