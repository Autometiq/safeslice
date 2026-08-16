// Copyright 2026 Autometiq
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/graph"
	"github.com/Autometiq/safeslice/internal/mask"
	"github.com/Autometiq/safeslice/internal/profile"
	"github.com/Autometiq/safeslice/internal/report"
	"github.com/Autometiq/safeslice/internal/ui"
)

// Running `safeslice` with no arguments opens this wizard.
//
// The commands are still the product -- they are what CI runs and what anyone
// scripting the tool uses -- but a first-time user faced with five of them and
// a YAML file has to read documentation before anything happens. The wizard
// carries them through init, plan, run and verify in one pass, asking only for
// the decisions a program cannot make: which columns hold personal data, how
// much data to take, and where it is allowed to go.
//
// Nothing here is a second implementation of the pipeline. The wizard fills in
// the same config the commands read and drives `execute` through hooks, so a
// slice built by the wizard and a slice built by `safeslice run` are the same
// slice.

// wizard is one session's answers.
type wizard struct {
	ctx    context.Context
	cfg    *config.Config
	cat    *catalog.Catalog
	store  *profile.Store
	source string // carries credentials: never printed, never written to disk
	target string
	// created names the database the wizard made itself, so the summary can say
	// so and the profile can recreate it.
	created bool
	// decided remembers a column-name decision so the same `notes` column in
	// five tables is not five separate questions. setKeys are the config keys
	// this session wrote, so "change masking" can take them back.
	decided map[string]mask.Rule
	setKeys []string
	// prefill is the profile the answers came from, if any.
	prefill *profile.Profile
	// reportDir is where the artifacts go.
	reportDir string
	// profileName is set when the run came from, or was saved as, a profile.
	profileName string
}

// planChange is what the user chose from the review screen.
type planChange int

const (
	planGo planChange = iota
	planMasking
	planSlice
	planTarget
	planCancel
)

// changePlan aborts a run in progress so an answer can be revisited. It is an
// error only in the sense that it unwinds the pipeline: nothing has been
// written to the target when it fires.
type changePlan struct{ what planChange }

func (c changePlan) Error() string { return "the plan was changed before anything was written" }

func runWizard(ctx context.Context) error {
	ui.Splash(resolveVersion())
	for {
		choice := ui.Select("What would you like to do?", []ui.Option{
			{Label: "Create a safe development database", Note: "the whole workflow, start to finish"},
			{Label: "Inspect an existing configuration", Note: "what a run would do; reads no data"},
			{Label: "Verify an existing database", Note: "scan for personal data that survived"},
			{Label: "Run demo", Note: "throwaway database — nothing of yours is touched"},
			{Label: "Advanced / CLI mode", Note: "the commands behind all of this"},
			{Label: "Quit", Note: ""},
		}, 0)

		var err error
		switch choice {
		case 0:
			// A finished run ends at its own results screen. Asking "back to
			// the menu?" after that sends someone who has just got what they
			// came for back to a list of things they no longer need.
			if err = createFlow(ctx); err == nil {
				return nil
			}
		case 1:
			err = inspectFlow(ctx)
		case 2:
			err = verifyFlow(ctx)
		case 3:
			err = demoFlow(ctx)
		case 4:
			showCommands()
		default:
			return nil
		}
		if err != nil {
			// A failed step returns to the menu rather than killing the process:
			// the user has answered several questions by now and losing them to
			// a typo in a connection string is its own kind of rude.
			ui.Fatal(err)
		}
		if !ui.Confirm("\nBack to the menu?") {
			return nil
		}
	}
}

// createFlow is the wizard proper: source, schema, decisions, destination,
// review, run, verify, report.
func createFlow(ctx context.Context) error {
	w := &wizard{
		ctx:       ctx,
		store:     profile.Open(""),
		decided:   map[string]mask.Rule{},
		reportDir: report.DefaultDir,
	}
	saved := w.choosePrefill()
	if err := w.chooseSource(); err != nil {
		return err
	}
	if saved != nil {
		ui.Detail("Slice settings came from the %q profile.", saved.Name)
	}
	if err := w.discover(); err != nil {
		return err
	}
	if err := w.classify(); err != nil {
		return err
	}
	w.chooseSlice()
	if err := w.chooseTarget(); err != nil {
		return err
	}
	if err := w.writeConfig(); err != nil {
		return err
	}

	// The review screen can send the user back to any earlier answer. Nothing
	// has been written to the target at that point, so unwinding is free.
	for {
		res, err := w.run()
		var chg changePlan
		if errors.As(err, &chg) {
			switch chg.what {
			case planMasking:
				if err := w.reclassify(); err != nil {
					return err
				}
			case planSlice:
				w.chooseSlice()
			case planTarget:
				if err := w.chooseTarget(); err != nil {
					return err
				}
			default:
				ui.Info("cancelled — nothing was written to %s", describe(w.target))
				return nil
			}
			if err := w.writeConfig(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		w.finish(res)
		return nil
	}
}

// choosePrefill offers the answers from a previous run. Returning to a project
// a week later should be one keystroke, not the same twenty questions.
func (w *wizard) choosePrefill() *profile.Profile {
	profiles, err := w.store.Profiles()
	if err != nil || len(profiles) == 0 {
		return nil
	}
	opts := make([]ui.Option, 0, len(profiles)+1)
	last := w.store.LastProfile()
	def := len(profiles) // "start fresh" unless one was used before
	for i, p := range profiles {
		note := p.Source.DSN
		if p.Root != "" {
			note = fmt.Sprintf("%s from %s", ui.Count(p.Limit), p.Root)
		}
		opts = append(opts, ui.Option{Label: p.Name, Note: note})
		if p.Name == last {
			def = i
		}
	}
	opts = append(opts, ui.Option{Label: "Start fresh", Note: "answer everything again"})

	i := ui.Select("Choose a profile", opts, def)
	if i < 0 || i == len(profiles) {
		return nil
	}
	p := profiles[i]
	w.profileName = p.Name
	w.prefill = &p
	return &p
}

// applyPrefill copies a profile's slice settings over a freshly loaded config.
// The masking rules deliberately come from safeslice.yaml, not from here: the
// config file is what the team reviews and commits.
func (w *wizard) applyPrefill() {
	p := w.prefill
	if p == nil {
		return
	}
	if p.Root != "" {
		w.cfg.Slice.Root = p.Root
	}
	if p.Limit > 0 {
		w.cfg.Slice.Limit = p.Limit
	}
	if p.Seed != "" {
		w.cfg.Mask.Seed = p.Seed
	}
	w.cfg.Slice.Where = p.Where
	w.cfg.Slice.ChildDepth = p.ChildDepth
}

// reclassify forgets the decisions made in this session and asks again. Used
// by "Change masking" on the review screen, where re-running classify() alone
// would find nothing left to ask about.
func (w *wizard) reclassify() error {
	for _, k := range w.setKeys {
		delete(w.cfg.Mask.Rules, k)
	}
	w.setKeys = nil
	w.decided = map[string]mask.Rule{}
	return w.classify()
}

// chooseSource connects to the database the slice comes from, retrying until
// it works or the user gives up.
func (w *wizard) chooseSource() error {
	ui.Section("Source")
	ui.Detail("Point this at a read replica if you have one. safeslice opens the")
	ui.Detail("source read-only and never writes to it.")

	dsn := ""
	for {
		if dsn == "" {
			var err error
			if dsn, err = w.askSource(); err != nil {
				return err
			}
		}
		conn, err := connectSource(w.ctx, dsn)
		if err == nil {
			summarise("Source", dsn)
			ui.Detail("")
			ui.Detail("%s", ui.Check("Connection successful"))
			if v := serverVersion(w.ctx, conn); v != "" {
				ui.Detail("%s", ui.Check("PostgreSQL %s detected", v))
			}
			ui.Detail("%s", ui.Check("Source opened read-only — nothing here will be modified"))
			conn.Close(context.Background())
			w.source = dsn
			return nil
		}
		switch explainConnection(dsn, err) {
		case connectRetry:
		case connectChange:
			dsn = ""
		case connectPassword:
			dsn = profile.WithPassword(dsn, ui.Password("Password (not echoed, not stored):"))
		default:
			return errors.New("cancelled at the source connection")
		}
	}
}

// askSource offers the ways of naming a database that do not involve typing a
// password into a terminal.
func (w *wizard) askSource() (string, error) {
	opts := []ui.Option{{Label: "Enter a connection string", Note: "postgres://user@host:5432/dbname"}}
	if w.prefill != nil && w.prefill.Source.DSN != "" {
		opts = append(opts, ui.Option{Label: "Use the profile's source",
			Note: w.prefill.Source.DSN})
	}
	env := os.Getenv("SAFESLICE_SOURCE")
	if env == "" {
		env = os.Getenv("DATABASE_URL")
	}
	if env != "" {
		opts = append(opts, ui.Option{Label: "Use SAFESLICE_SOURCE", Note: describe(env)})
	}
	saved, _ := w.store.Connections()
	if len(saved) > 0 {
		opts = append(opts, ui.Option{Label: "Choose from saved connections",
			Note: fmt.Sprintf("%d saved in %s", len(saved), w.store.Path())})
	}

	// The default is whichever answer the environment has already given.
	def := 0
	for i, o := range opts {
		if o.Label == "Use SAFESLICE_SOURCE" || o.Label == "Use the profile's source" {
			def = i
			break
		}
	}
	switch label(opts, ui.Select("Where is your source PostgreSQL database?", opts, def)) {
	case "Use SAFESLICE_SOURCE":
		return env, nil
	case "Use the profile's source":
		// The stored string has no password, by design. Postgres will look in
		// .pgpass and PGPASSWORD as usual, and the retry loop asks if it must.
		return w.prefill.Source.Resolve(), nil
	case "Choose from saved connections":
		return w.pickConnection(saved)
	case "Enter a connection string":
		dsn := ui.Ask("\nConnection string:", "")
		if dsn == "" {
			return "", ui.Hint(errors.New("no source given"),
				"A connection string looks like postgres://user:password@host:5432/dbname")
		}
		return dsn, nil
	}
	return "", errors.New("cancelled at the source connection")
}

func (w *wizard) pickConnection(saved []profile.Connection) (string, error) {
	opts := make([]ui.Option, 0, len(saved))
	for _, c := range saved {
		opts = append(opts, ui.Option{Label: c.Name, Note: c.DSN})
	}
	i := ui.Select("Saved connections", opts, 0)
	if i < 0 {
		return "", errors.New("cancelled at the source connection")
	}
	dsn := saved[i].Resolve()
	if saved[i].PasswordEnv != "" && os.Getenv(saved[i].PasswordEnv) == "" {
		ui.Warn("%s is empty; the connection may need a password", saved[i].PasswordEnv)
	}
	return dsn, nil
}

// discover reads the schema and reports what is there, in five numbers rather
// than five hundred lines.
func (w *wizard) discover() error {
	conn, err := connectSource(w.ctx, w.source)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	w.cfg, err = config.Load(flagConfig)
	if err != nil {
		return err
	}
	if len(flagSchemas) > 0 {
		w.cfg.Source.Schemas = flagSchemas
	}

	w.applyPrefill()

	live := ui.Start("reading the schema")
	w.cat, err = catalog.Load(w.ctx, conn, w.cfg.Source.Schemas)
	live.Stop()
	if err != nil {
		return err
	}
	if len(w.cat.Tables) == 0 {
		return ui.Hint(fmt.Errorf("no tables found in %s", strings.Join(w.cfg.Source.Schemas, ", ")),
			"Pass --schema if your tables live somewhere other than `public`.")
	}

	auto, review := w.columnCounts()
	var rows int64
	for _, t := range w.cat.Tables {
		if !t.Partitioned {
			rows += t.EstRows
		}
	}

	// reltuples is maintained by ANALYZE, so a freshly restored database reports
	// nothing. Printing "0" against a table full of rows reads as a bug.
	estimate := ui.Count(int(rows))
	if rows <= 0 {
		estimate = "not analysed yet"
	}
	ui.Section("Database discovered")
	ui.Table([]string{"", ""}, [][]string{
		{"Tables", ui.Count(len(w.cat.Refs()))},
		{"Relationships", ui.Count(len(w.cat.FKs))},
		{"Estimated rows", estimate},
		{"", ""},
		{"Sensitive columns found", ui.Count(auto + len(review))},
		{"Classified automatically", ui.Count(auto)},
		{"Need your decision", ui.Count(len(review))},
	})
	if rows > 0 {
		ui.Detail("Row counts are PostgreSQL's own estimates, not a full count.")
	}
	// Adopting a config silently would leave the user wondering why they were
	// never asked about a column they remember deciding on.
	if n := len(w.cfg.Mask.Rules); n > 0 {
		ui.Detail("Using the %d rules already in %s.", n, config.DefaultPath)
	}
	return nil
}

// columnCounts splits the columns into the ones name-matching handled and the
// ones a human still owns.
func (w *wizard) columnCounts() (auto int, review []undecided) {
	cl := w.cfg.Classifier()
	for _, ref := range w.cat.Refs() {
		t, ok := w.cat.Table(ref)
		if !ok || t.Partition {
			continue // counted once, on the parent
		}
		keys := w.cat.KeyColumns(ref)
		for _, col := range t.Columns {
			if keys[col.Name] || !col.Insertable() {
				continue
			}
			if cl.Rule(ref, col.Name) != mask.Keep {
				auto++
			}
		}
		unique := mask.UniqueColumns(t)
		for _, name := range cl.Unclassified(t, keys) {
			col, _ := t.Column(name)
			review = append(review, undecided{Table: ref, Column: col, Unique: unique[name]})
		}
	}
	sort.Slice(review, func(i, j int) bool {
		if review[i].Table != review[j].Table {
			return review[i].Table.String() < review[j].Table.String()
		}
		return review[i].Column.Name < review[j].Column.Name
	})
	return auto, review
}

// undecided is one column nobody has ruled on.
type undecided struct {
	Table  catalog.Ref
	Column catalog.Column
	// Unique marks a column under a unique constraint, which rules out
	// dropping the value: every row would end up identical.
	Unique bool
}

func (u undecided) key() string { return u.Table.Name + "." + u.Column.Name }

// classify walks the columns the classifier could not judge.
//
// This is the only part of the wizard that cannot be skipped or defaulted away.
// A text column nobody looked at is exactly the one that carries a customer's
// name into a laptop, so the choices are explicit, the recommendation is
// explained, and "keep" is never the silent default.
func (w *wizard) classify() error {
	_, review := w.columnCounts()
	if len(review) == 0 {
		ui.Success("every column was classified automatically")
		return nil
	}

	ui.Section("Columns that need a decision")
	ui.Detail("%d text columns hold values safeslice cannot judge from the name alone.", len(review))
	ui.Detail("Each one is a decision you own. Nothing is treated as safe by default.")
	ui.Detail("")

	switch ui.Select("How do you want to handle them?", []ui.Option{
		{Label: "Accept all recommended choices", Note: "safest option, one keystroke",
			Body: []string{"Free text is redacted; short constrained values are kept."}},
		{Label: "Review individually", Note: fmt.Sprintf("%d questions", len(review))},
		{Label: "Stop and edit safeslice.yaml myself", Note: "nothing is read from your tables"},
	}, 0) {
	case 0:
		for _, u := range review {
			rule, why := recommend(u)
			w.setRule(u, rule)
			ui.Detail("%-34s %s   %s", u.key(), rule, why.short)
		}
		ui.Success("%d columns classified", len(review))
		return nil
	case 1:
		for i, u := range review {
			if err := w.decide(i, len(review), u); err != nil {
				return err
			}
		}
		return nil
	default:
		return ui.HintCmd(errors.New("stopped before reading any data"),
			"Give every listed column a rule, then run the wizard again.",
			"$EDITOR "+config.DefaultPath)
	}
}

// reason explains a recommendation, in one line and in full.
type reason struct {
	short string
	long  []string
}

// recommend picks a rule and says why.
//
// Free text gets redacted because no regex can reliably scrub a support ticket
// that happens to quote a customer's address. Short constrained values are the
// only ones recommended for keeping, and even then the reader is told that is a
// judgement they are making, not a fact safeslice established.
func recommend(u undecided) (mask.Rule, reason) {
	name := strings.ToLower(u.Column.Name)
	// A unique column cannot be emptied: every row would hold the same value
	// and the constraint would reject the second one. Replacing it keeps both
	// the privacy and the schema.
	if u.Unique && mask.SatisfiesUnique(mask.Redact, u.Column) != nil {
		return mask.Secret, reason{
			short: "unique, cannot be emptied",
			long: []string{
				"This column is UNIQUE and NOT NULL. Dropping the value would make every",
				"row identical and the load would fail, so the value is replaced with a",
				"generated one instead — distinct per row, and not the original.",
			}}
	}
	for _, hint := range []string{"body", "text", "note", "comment", "description",
		"content", "message", "summary", "bio", "about", "reason", "feedback", "answer"} {
		if strings.Contains(name, hint) {
			return mask.Redact, reason{
				short: "free text",
				long: []string{
					"This is free-form user-generated text. It may contain names, emails,",
					"phone numbers, addresses or anything else a person typed into a box.",
					"No pattern-matching can clean that reliably, so the safe answer is to",
					"drop the value rather than pretend it was scrubbed.",
				}}
		}
	}
	if u.Column.MaxLen > 0 && u.Column.MaxLen <= 32 {
		return mask.Keep, reason{
			short: fmt.Sprintf("varchar(%d), looks constrained", u.Column.MaxLen),
			long: []string{
				fmt.Sprintf("Declared as varchar(%d), which is the shape of a status, a code", u.Column.MaxLen),
				"or a short label rather than something a person wrote. Keeping it is a",
				"judgement you are making: if it can hold a name or a reference number,",
				"choose redact instead.",
			}}
	}
	return mask.Redact, reason{
		short: "unbounded text",
		long: []string{
			"An unbounded text column with a name that gives nothing away. safeslice",
			"cannot tell whether this holds personal data, and the cost of being wrong",
			"is production data on a laptop, so the recommendation is to drop it.",
		}}
}

// decide asks about one column.
func (w *wizard) decide(i, total int, u undecided) error {
	rule, why := recommend(u)
	if prior, ok := w.decided[u.Column.Name]; ok {
		rule = prior // the same column name in another table, already answered
		why = reason{short: "matches your earlier choice for " + u.Column.Name}
	}

	ui.Section(fmt.Sprintf("Column %d of %d", i+1, total))
	ui.Detail("%s", ui.Field("Column", u.Table.Name+"."+u.Column.Name))
	ui.Detail("%s", ui.Field("Type", strings.ToUpper(u.Column.Type)))
	if u.Column.NotNull {
		ui.Detail("%s", ui.Field("", "NOT NULL — a redacted value becomes an empty string, not null"))
	}
	ui.Detail("")
	ui.Detail("%s", ui.Alert("Recommended: %s   (%s)", strings.ToUpper(string(rule)), why.short))
	for _, l := range why.long {
		ui.Detail("%s", l)
	}
	ui.Detail("")

	opts := []ui.Option{
		{Label: "Redact", Note: "safest", Body: []string{"Drops the original value entirely."}},
		{Label: "Replace with a generated secret", Note: "",
			Body: []string{"Substitutes a fake value, so the fact that a value existed survives."}},
		{Label: "Keep unchanged", Note: "",
			Body: []string{"The original data reaches the development database. Choose this only",
				"if you are confident the column holds nothing personal."}},
		{Label: "Custom rule", Note: "email, phone, name, address, govid, ip"},
	}
	choice := ui.Select("What should happen to it?", opts, indexFor(rule))
	switch choice {
	case 0:
		// Chosen against the recommendation, this fails the load rather than
		// producing a bad slice -- but it fails minutes later, so say so now.
		if err := mask.SatisfiesUnique(mask.Redact, u.Column); err != nil && u.Unique {
			ui.Warn("%s", err)
		}
		w.setRule(u, mask.Redact)
	case 1:
		w.setRule(u, mask.Secret)
	case 2:
		w.setRule(u, mask.Keep)
	case 3:
		w.setRule(u, w.askCustomRule())
	default:
		return errors.New("cancelled while classifying columns")
	}
	return nil
}

// indexFor maps a recommended rule onto the option that applies it.
func indexFor(r mask.Rule) int {
	switch r {
	case mask.Secret:
		return 1
	case mask.Keep:
		return 2
	default:
		return 0
	}
}

func (w *wizard) askCustomRule() mask.Rule {
	rules := []mask.Rule{mask.Email, mask.Phone, mask.FullName, mask.FirstName,
		mask.LastName, mask.Address, mask.GovID, mask.IP}
	opts := make([]ui.Option, len(rules))
	for i, r := range rules {
		opts[i] = ui.Option{Label: string(r), Note: maskExample(r)}
	}
	i := ui.Select("Replace it with what?", opts, 0)
	if i < 0 {
		return mask.Redact
	}
	return rules[i]
}

// setRule records a decision in the config and remembers it for columns of the
// same name elsewhere.
func (w *wizard) setRule(u undecided, r mask.Rule) {
	if w.cfg.Mask.Rules == nil {
		w.cfg.Mask.Rules = map[string]string{}
	}
	w.cfg.Mask.Rules[u.key()] = string(r)
	w.setKeys = append(w.setKeys, u.key())
	w.decided[u.Column.Name] = r
}

// chooseSlice asks how much data to take and where to start.
func (w *wizard) chooseSlice() {
	ui.Section("How much data do you want?")
	ui.Detail("safeslice starts from one table and follows its relationships. Every")
	ui.Detail("row it takes brings the rows it references with it, so the slice")
	ui.Detail("restores without foreign-key errors — which is why the total is always")
	ui.Detail("larger than the number you pick here.")
	ui.Detail("")

	sizes := []struct {
		label, note string
		body        []string
		limit, deep int
	}{
		{"Small development slice", "~1,000 root rows",
			[]string{"Fastest. Enough to click through an application locally."}, 1_000, 1},
		{"Medium development slice", "~10,000 root rows",
			[]string{"Realistic testing, still seconds to load."}, 10_000, 1},
		{"Large development slice", "~100,000 root rows",
			[]string{"Performance work. Minutes, and a few GB depending on your schema."}, 100_000, 2},
		{"Custom", "choose the numbers yourself", nil, 0, 0},
	}
	opts := make([]ui.Option, len(sizes))
	for i, s := range sizes {
		opts[i] = ui.Option{Label: s.label, Note: s.note, Body: s.body}
	}
	pick := ui.Select("Slice size", opts, 0)
	if pick < 0 {
		pick = 0
	}
	if s := sizes[pick]; s.limit > 0 {
		w.cfg.Slice.Limit, w.cfg.Slice.ChildDepth = s.limit, s.deep
	} else {
		w.cfg.Slice.Limit = ui.AskInt("Rows from the root table:", 1000)
		w.cfg.Slice.ChildDepth = ui.AskInt("Levels of child rows to follow:", 1)
	}

	w.chooseRoot()

	ui.Detail("")
	ui.Detail("A filter narrows the starting rows: `id = 4821` slices around one")
	ui.Detail("customer, `created_at > '2026-01-01'` around a date range. Leave it")
	ui.Detail("empty to take the first %s rows.", ui.Count(w.cfg.Slice.Limit))
	w.cfg.Slice.Where = ui.Ask("\nFilter (optional):", w.cfg.Slice.Where)
}

// chooseRoot offers the tables most of the schema points at.
func (w *wizard) chooseRoot() {
	incoming := map[string]int{}
	for _, fk := range w.cat.FKs {
		incoming[fk.RefTable.Name]++
	}
	var names []string
	for _, ref := range w.cat.Refs() {
		if t, ok := w.cat.Table(ref); ok && t.Partition {
			continue
		}
		names = append(names, ref.Name)
	}
	sort.Slice(names, func(i, j int) bool {
		if incoming[names[i]] != incoming[names[j]] {
			return incoming[names[i]] > incoming[names[j]]
		}
		return names[i] < names[j]
	})

	opts := make([]ui.Option, 0, 5)
	for _, n := range names[:min(4, len(names))] {
		note := "referenced by nothing"
		if c := incoming[n]; c > 0 {
			note = fmt.Sprintf("referenced by %d table(s)", c)
		}
		opts = append(opts, ui.Option{Label: n, Note: note})
	}
	opts = append(opts, ui.Option{Label: "Another table", Note: "type the name"})

	def := 0
	for i, o := range opts {
		if o.Label == w.cfg.Slice.Root {
			def = i
		}
	}
	i := ui.Select("Which table should the slice start from?", opts, def)
	switch {
	case i < 0:
		return
	case i == len(opts)-1:
		w.cfg.Slice.Root = ui.Ask("Table name:", w.cfg.Slice.Root)
	default:
		w.cfg.Slice.Root = opts[i].Label
	}
}

// maskingPreview shows what will be transformed, with an example built from
// invented input. Demonstrating masking on a real row would mean reading and
// printing production data to prove production data is not printed.
func (w *wizard) maskingPreview() {
	rules, redacted, unreviewed := maskingRules(w.cat, w.cfg)
	ui.Section("Masking preview")
	if len(rules)+len(redacted) == 0 {
		ui.Warn("no columns matched a masking rule — check the config before trusting this slice")
		return
	}
	var rows [][]string
	for _, r := range append(append([]report.Rule{}, rules...), redacted...) {
		rows = append(rows, []string{r.Column, "→ " + r.Rule})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	ui.Table([]string{"COLUMN", "RULE"}, rows)
	ui.Detail("")
	ui.Detail("%d columns will be transformed, %d dropped.", len(rules), len(redacted))
	if len(unreviewed) > 0 {
		ui.Warn("%d columns will pass through unreviewed", len(unreviewed))
	}

	// One worked example, from a made-up value.
	m := mask.Masker{Seed: w.cfg.Mask.Seed}
	col := catalog.Column{Name: "email", Type: "text", MaxLen: -1}
	in := "john@example.com"
	if out, err := m.Value(mask.Email, col, &in, 0); err == nil && out != nil {
		ui.Detail("")
		ui.Detail("%s", ui.Field("Example input", in+"   (invented, not from your data)"))
		ui.Detail("%s", ui.Field("Becomes", *out))
		ui.Detail("%s", ui.Field("", "the same input always becomes the same output for seed "+w.cfg.Mask.Seed))
	}
}

// maskExample renders one rule's output shape, for the custom-rule menu.
func maskExample(r mask.Rule) string {
	m := mask.Masker{Seed: "safeslice"}
	in := "example"
	out, err := m.Value(r, catalog.Column{Name: "sample", Type: "text", MaxLen: -1}, &in, 0)
	if err != nil || out == nil {
		return ""
	}
	return "e.g. " + *out
}

// chooseTarget decides where the slice goes, and makes sure that place exists
// and has the tables to receive it.
func (w *wizard) chooseTarget() error {
	w.maskingPreview()

	ui.Section("Destination")
	ui.Detail("safeslice copies rows, not table definitions. The target needs the")
	ui.Detail("same schema — from your own migrations, or copied from the source.")

	// An option that cannot work is worse than one absent: offering to create a
	// local database when nothing is listening only produces a dead end.
	local, hasLocal := localServer(w.ctx)
	var opts []ui.Option
	if hasLocal {
		opts = append(opts, ui.Option{Label: "Create a local database automatically",
			Note: localNote(local)})
	}
	opts = append(opts,
		ui.Option{Label: "Enter a PostgreSQL connection", Note: "a server you name yourself"},
		ui.Option{Label: "Use an existing database", Note: "already migrated and ready"},
		ui.Option{Label: "Docker PostgreSQL", Note: "a container safeslice starts for you"})

	var err error
	switch label(opts, ui.Select("Where should safeslice create the development database?", opts, 0)) {
	case "Create a local database automatically":
		err = w.createLocal(local)
	case "Enter a PostgreSQL connection", "Use an existing database":
		dsn := ui.Ask("\nTarget connection string:", "postgres://localhost:5432/myapp_dev")
		if dsn == "" {
			return errors.New("no target given")
		}
		w.target = dsn
		err = w.prepareTarget(false)
	case "Docker PostgreSQL":
		err = w.createDocker()
	default:
		return errors.New("cancelled at the destination")
	}
	if err != nil {
		return err
	}

	if err := refuseSameDatabase(w.source, w.target); err != nil {
		return err
	}
	summarise("Target", w.target)
	ui.Detail("")
	ui.Detail("%s", ui.Check("Ready"))
	return nil
}

func localNote(dsn string) string {
	e, _ := parseEndpoint(dsn)
	return fmt.Sprintf("detected on %s:%s as %s", e.Host, e.Port, e.User)
}

// createLocal names and creates a database on a server the user already runs.
func (w *wizard) createLocal(admin string) error {
	src, _ := parseEndpoint(w.source)
	name, err := askDatabaseName(defaultTargetName(src.Database))
	if err != nil {
		return err
	}

	for {
		exists, err := databaseExists(w.ctx, admin, name)
		if err != nil {
			return err
		}
		if !exists {
			break
		}
		// An existing database is somebody's work until they say otherwise.
		next, done, err := w.resolveExisting(admin, name)
		if err != nil {
			return err
		}
		if done {
			break
		}
		name = next
	}

	dsn, err := withDatabase(admin, name)
	if err != nil {
		return err
	}
	if exists, _ := databaseExists(w.ctx, admin, name); !exists {
		live := ui.Start("creating %s", name)
		err := createDatabase(w.ctx, admin, name)
		live.Stop()
		if err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
		ui.Success("created %s", name)
		w.created = true
	}
	w.target = dsn
	return w.prepareTarget(w.created)
}

// resolveExisting handles a name that is already taken. Returns the name to try
// next, or done when the current one may be used as it stands.
func (w *wizard) resolveExisting(admin, name string) (string, bool, error) {
	ui.Warn("database %q already exists", name)
	switch ui.Select("What should happen to it?", []ui.Option{
		{Label: "Use it as it is", Note: "load into the existing schema",
			Body: []string{"Rows are added to what is already there."}},
		{Label: fmt.Sprintf("Create %q instead", name+"_2"), Note: "leaves the original alone"},
		{Label: "Choose another name", Note: ""},
		{Label: "Replace it", Note: "destroys everything in it",
			Body: []string{"DROP DATABASE, then recreate it empty. This cannot be undone."}},
		{Label: "Cancel", Note: ""},
	}, 0) {
	case 0:
		return name, true, nil
	case 1:
		return name + "_2", false, nil
	case 2:
		next := ui.Ask("Database name:", name+"_dev")
		if next == "" || next == name {
			return name, false, errors.New("no new name given")
		}
		return next, false, nil
	case 3:
		// Typing the name is the confirmation. A y/N on a destructive action is
		// answered by reflex; typing the database out is not.
		ui.Warn("this permanently deletes every table and row in %q", name)
		if ui.Ask(fmt.Sprintf("Type %s to confirm:", name), "") != name {
			return name, false, errors.New("not confirmed — nothing was deleted")
		}
		live := ui.Start("dropping %s", name)
		err := dropDatabase(w.ctx, admin, name)
		live.Stop()
		if err != nil {
			return name, false, fmt.Errorf("dropping %s: %w", name, err)
		}
		ui.Success("dropped %s", name)
		w.created = true
		return name, false, nil
	default:
		return name, false, errors.New("cancelled at the destination")
	}
}

// createDocker runs the target in a container.
func (w *wizard) createDocker() error {
	src, _ := parseEndpoint(w.source)
	name, err := askDatabaseName(defaultTargetName(src.Database))
	if err != nil {
		return err
	}
	live := ui.Start("starting PostgreSQL in Docker")
	dsn, err := dockerTarget(w.ctx, name, func(s string) { live.Detail("%s", s) })
	live.Stop()
	if err != nil {
		return err
	}
	ui.Success("container %s ready on port %s", targetContainer, targetPort)
	ui.Detail("stop it with: docker stop %s", targetContainer)
	w.target = dsn
	w.created = true
	return w.prepareTarget(true)
}

// prepareTarget makes sure the destination can actually receive the slice.
//
// The failure this prevents is the one every new user hits: safeslice loads
// rows into tables that have to exist already, so an empty database fails
// halfway through with `relation "users" does not exist`.
func (w *wizard) prepareTarget(fresh bool) error {
	for {
		missing, err := missingTables(w.ctx, w.target, w.cat, w.cfg.Source.Schemas)
		if err != nil {
			return ui.Hint(err, "Could not read the target's schema. "+causesFor(err))
		}
		if len(missing) == 0 {
			ui.Success("the target already has every table the slice needs")
			return nil
		}

		ui.Warn("%d of %d tables are missing from the target", len(missing), len(w.cat.Refs()))
		for _, m := range missing[:min(5, len(missing))] {
			ui.Detail("%s", m)
		}
		if len(missing) > 5 {
			ui.Detail("and %d more", len(missing)-5)
		}

		def := 1
		if fresh {
			def = 0 // a database safeslice just created has no migrations to run
		}
		switch ui.Select("How should the schema get there?", []ui.Option{
			{Label: "Copy the structure from the source", Note: "uses pg_dump --schema-only",
				Body: []string{"Table definitions only. No rows leave the source through this."}},
			{Label: "I will run my own migrations now", Note: "then check again"},
			{Label: "Load anyway", Note: "fails if a table is genuinely missing"},
			{Label: "Cancel", Note: ""},
		}, def) {
		case 0:
			live := ui.Start("copying the schema")
			err := copySchema(w.ctx, w.source, w.target)
			live.Stop()
			if err != nil {
				return err
			}
			ui.Success("schema copied")
		case 1:
			ui.Detail("Run your migrations against %s, then press enter.", describe(w.target))
			ui.Ask("Press enter when ready:", "")
		case 2:
			return nil
		default:
			return errors.New("cancelled at the destination")
		}
	}
}

func defaultTargetName(source string) string {
	if source == "" {
		return "myapp_dev"
	}
	return strings.TrimSuffix(source, "_prod") + "_dev"
}

// databaseName is what PostgreSQL accepts unquoted. Anything else is almost
// certainly a mistyped answer to the previous question, and creating a database
// called "1" helps nobody.
var databaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

func askDatabaseName(def string) (string, error) {
	for range 3 {
		name := ui.Ask("\nName for the development database:", def)
		switch {
		case name == "":
			return "", errors.New("no database name given")
		case databaseName.MatchString(name):
			return name, nil
		}
		ui.Warn("a database name starts with a letter and holds letters, digits and underscores")
	}
	return "", errors.New("no usable database name given")
}

// writeConfig saves the answers as a safeslice.yaml, so the same slice can be
// repeated by `safeslice run` without the wizard, in CI, by anyone on the team.
func (w *wizard) writeConfig() error {
	path := flagConfig
	if path == "" {
		path = config.DefaultPath
	}
	body, err := yaml.Marshal(w.cfg)
	if err != nil {
		return err
	}
	header := "# safeslice configuration, written by the wizard.\n" +
		"# Commit this file: it is what gives every teammate and every CI job the\n" +
		"# same masking rules and the same slice.\n" +
		"#\n# Repeat this run with:  safeslice run --to \"postgres://localhost/" +
		targetDatabase(w.target) + "\"\n\n"
	if err := os.WriteFile(path, append([]byte(header), body...), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func targetDatabase(dsn string) string {
	e, err := parseEndpoint(dsn)
	if err != nil {
		return "myapp_dev"
	}
	return e.Database
}

// run drives the pipeline, showing a checklist rather than a log and stopping
// at the review screen before anything is read or written.
func (w *wizard) run() (report.Result, error) {
	var steps *ui.Steps
	h := &runHooks{}
	h.detail = func(s string) { steps.Detail("%s", s) }
	h.rows = func(done, total int) {
		steps.Detail("%s of %s rows", ui.Count(done), ui.Count(total))
		if total > 0 {
			steps.Percent(done * 100 / total)
		}
	}
	h.stage = func(st runStage) {
		switch st {
		case stageSelected:
			steps.Advance()
			steps.Done() // the review screen prompts next; freeze the block
		case stageReported:
			steps.Advance()
			steps.Done()
		default:
			steps.Advance()
		}
	}
	h.confirm = func(counts map[string]int, total int) error {
		if err := w.review(counts, total); err != nil {
			return err
		}
		if flagVerbose {
			return nil
		}
		steps = ui.NewSteps("Creating your safe development database",
			"Extracting and masking rows",
			"Loading the target database",
			"Verifying the result",
			"Writing the report")
		return nil
	}

	// --verbose hands the display back to the pipeline: a nil Steps ignores
	// every call, and the run prints the log it prints for the CLI.
	h.verbose = flagVerbose
	if !flagVerbose {
		steps = ui.NewSteps("Reading your database",
			"Connecting to the source",
			"Reading the schema",
			"Resolving relationships",
			"Selecting rows")
	}

	res, err := execute(w.ctx, runRequest{
		cfg: w.cfg, source: w.source, target: w.target,
		reportDir: w.reportDir, hooks: h,
	})
	if err != nil {
		var chg changePlan
		if errors.As(err, &chg) {
			steps.Done()
		} else {
			steps.Fail()
		}
		return res, err
	}
	steps.Done()
	return res, nil
}

// review is the last screen before any data moves.
func (w *wizard) review(counts map[string]int, total int) error {
	rules, redacted, unreviewed := maskingRules(w.cat, w.cfg)
	soft := graph.SoftKeys(w.cat, append(append([]catalog.FK{}, w.cat.FKs...), w.cfg.FKs()...))

	ui.Section("Slice preview")
	rows := make([][]string, 0, len(counts))
	root := w.cfg.Root().String()
	for name, n := range counts {
		label := name
		if name == root {
			label += "  (root)"
		}
		rows = append(rows, []string{label, ui.Count(n)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	rows = append(rows, []string{"", ""}, []string{"Total", ui.Count(total)})
	ui.Table([]string{"TABLE", "ROWS"}, rows)
	ui.Detail("")
	ui.Detail("%s", ui.Check("Foreign-key closure complete — every row brings its parents"))
	if len(soft) > 0 {
		// Listed rather than offered behind a prompt: this is the reason a slice
		// people expected to be bigger comes out small, and a question they can
		// press enter past is a question they will press enter past.
		ui.Detail("%s", ui.Alert("%d %s the database does not enforce, so they were not followed:",
			len(soft), plural(len(soft), "relationship", "relationships")))
		for _, s := range soft[:min(3, len(soft))] {
			ui.Detail("%s", s)
		}
		if len(soft) > 3 {
			ui.Detail("and %d more — all of them are listed in the report", len(soft)-3)
		}
		ui.Detail("Declare the ones that matter as virtual_keys in %s.", config.DefaultPath)
	}

	srcE, _ := parseEndpoint(w.source)
	tgtE, _ := parseEndpoint(w.target)
	body := []string{
		ui.Field("Source", srcE.Database+"   "+srcE.Host+":"+srcE.Port),
		ui.Field("Target", tgtE.Database+"   "+tgtE.Host+":"+tgtE.Port),
		ui.Field("Root", w.cfg.Slice.Root),
	}
	if w.cfg.Slice.Where != "" {
		body = append(body, ui.Field("Filter", w.cfg.Slice.Where))
	}
	body = append(body,
		"",
		ui.Field("Tables", ui.Count(len(counts))),
		ui.Field("Rows", ui.Count(total)),
		ui.Field("Masked", fmt.Sprintf("%d columns", len(rules))),
		ui.Field("Redacted", fmt.Sprintf("%d columns", len(redacted))),
		"",
		ui.Check("Source is read-only — it will not be modified"),
		ui.Check("Foreign-key integrity preserved"),
		ui.Check("Sensitive columns classified"),
	)
	if len(unreviewed) > 0 {
		body = append(body, ui.Alert("%d columns unreviewed — they pass through unchanged", len(unreviewed)))
	} else {
		body = append(body, ui.Check("Free text reviewed"))
	}
	ui.Box("SAFESLICE REVIEW", body)

	switch ui.Select("", []ui.Option{
		{Label: "Create the database", Note: "start reading and loading"},
		{Label: "Change masking", Note: ""},
		{Label: "Change slice size", Note: ""},
		{Label: "Change destination", Note: ""},
		{Label: "Cancel", Note: "nothing has been written"},
	}, 0) {
	case 0:
		return nil
	case 1:
		return changePlan{planMasking}
	case 2:
		return changePlan{planSlice}
	case 3:
		return changePlan{planTarget}
	default:
		return changePlan{planCancel}
	}
}

// finish reports the result and leaves the user with something to paste.
func (w *wizard) finish(res report.Result) {
	body := []string{
		"",
		ui.Check("Database created"),
		ui.Check("%s rows loaded", ui.Count(res.TotalRows)),
		ui.Check("%d columns masked", len(res.Rules)),
		ui.Check("%d columns redacted", len(res.Redacted)),
		ui.Check("0 foreign-key orphans"),
	}
	switch {
	case !res.Verification.Ran:
		body = append(body, ui.Alert("Privacy verification did not run"))
	case res.Verification.Passed:
		body = append(body, ui.Check("Privacy verification passed"))
	default:
		body = append(body, ui.Cross("Privacy scan flagged %d columns", len(res.Verification.Findings)))
	}
	if len(res.Unreviewed) > 0 {
		body = append(body, ui.Alert("%d columns were never reviewed", len(res.Unreviewed)))
	}
	body = append(body,
		"",
		ui.Field("Database", res.Target.Database),
		ui.Field("Connection", res.Target.URL()),
		ui.Field("Took", ui.Duration(res.Duration)),
	)
	ui.Box("SAFE DATABASE READY", body)

	// The caveat is repeated here on purpose. "Verification passed" means no
	// known pattern was found in the sampled rows; it is not a proof that no
	// personal data exists, and a reader who takes it as one will eventually be
	// wrong in a way that matters.
	if res.Verification.Ran && res.Verification.Passed {
		ui.Detail("%s", res.Verification.Caveat)
	}
	if len(res.Verification.Findings) > 0 {
		for _, f := range res.Verification.Findings {
			ui.Detail("%s", f)
		}
		ui.Detail("Add a rule for each in %s and run again.", config.DefaultPath)
	}

	ui.Section("Connect")
	for _, s := range report.Snippets(res.Target) {
		if strings.Contains(s.Code, "\n") {
			continue // the multi-line framework snippets live in the README
		}
		ui.Detail("%-14s %s", s.Name, s.Code)
	}
	ui.Detail("")
	ui.Detail("Prisma, Rails, Django and Node.js snippets are in the README.")

	w.record(res)
	w.offerProfile()
	showResults(w.reportDir, res.Target)
}

// showResults is the last screen: everything the run produced, one keystroke
// away.
//
// Nothing opens on its own. A tool that throws a browser window at you the
// moment it finishes has decided what you wanted next; this offers instead,
// and stays until you are done rather than dropping you back at the main menu
// to find your own way out.
func showResults(dir string, target report.Endpoint) {
	type artifact struct {
		label, note, file string
	}
	files := []artifact{
		{"Open the HTML report", "the full run, formatted — charts, tables, checks", "report.html"},
		{"Open the output folder", absOr(dir), ""},
		{"Copy the connection string", target.URL(), "-"},
		{"Open README.md", "the same summary in Markdown", "README.md"},
		{"Open tables.csv", "row counts per table, for a spreadsheet", "tables.csv"},
		{"Open summary.json", "machine-readable, for CI", "summary.json"},
		{"Open masking-rules.yaml", "the rules that were applied", "masking-rules.yaml"},
	}

	opts := make([]ui.Option, 0, len(files)+1)
	for _, f := range files {
		opts = append(opts, ui.Option{Label: f.label, Note: f.note})
	}
	opts = append(opts, ui.Option{Label: "Done", Note: "everything is on disk; nothing else to run"})

	for {
		i := ui.Select("", opts, 0)
		if i < 0 || i == len(files) {
			ui.Detail("")
			ui.Detail("Everything is in %s", absOr(dir))
			ui.Footer()
			return
		}
		f := files[i]
		switch {
		case f.file == "":
			if err := report.Reveal(dir); err != nil {
				ui.Warn("could not open the folder: %s", err)
				ui.Detail("%s", absOr(dir))
			}
		case f.file == "-":
			url := target.URL()
			if report.Clipboard(url) {
				ui.Success("copied  %s", url)
			} else {
				// No clipboard on a headless box; printing it is the fallback
				// that always works.
				ui.Detail("%s", url)
			}
		default:
			path := absOr(filepath.Join(dir, f.file))
			if err := report.Open(path); err != nil {
				ui.Warn("could not open it: %s", err)
				ui.Detail("%s", path)
			}
		}
	}
}

// record appends the run to the project history: what went where, and whether
// it passed. No credentials, no rows.
func (w *wizard) record(res report.Result) {
	_ = w.store.Record(profile.Run{
		Profile:   w.profileName,
		Source:    profile.Sanitise(w.source),
		Target:    profile.Sanitise(w.target),
		Tables:    len(res.Tables),
		Rows:      res.TotalRows,
		Masked:    len(res.Rules),
		Verified:  res.Verification.Ran && res.Verification.Passed,
		Artifacts: w.reportDir,
	})
}

// offerProfile saves the answers so the next run is one menu choice.
func (w *wizard) offerProfile() {
	if !ui.Confirm("\nSave this setup as a profile for next time?") {
		return
	}
	name := ui.Ask("Profile name:", "Local development")
	if name == "" {
		return
	}
	p := profile.Profile{
		Name:       name,
		Source:     profile.Connection{Name: name + " source", DSN: w.source},
		Target:     profile.Connection{Name: name + " target", DSN: w.target},
		Config:     config.DefaultPath,
		Root:       w.cfg.Slice.Root,
		Where:      w.cfg.Slice.Where,
		Limit:      w.cfg.Slice.Limit,
		ChildDepth: w.cfg.Slice.ChildDepth,
		Seed:       w.cfg.Mask.Seed,
	}
	if err := w.store.Save(p); err != nil {
		ui.Warn("could not save the profile: %s", err)
		return
	}
	_ = w.store.AddConnection(p.Source)
	w.profileName = name
	ui.Success("saved to %s", filepath.Join(w.store.Path(), "profiles"))
	ui.Detail("Passwords are never written here. Set one in the environment and")
	ui.Detail("name the variable with password_env if the source needs one.")
}

// inspectFlow shows what a run would do without reading any data.
func inspectFlow(ctx context.Context) error {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return err
	}
	if cfg.Slice.Root == "" {
		return ui.HintCmd(errors.New("no configuration found"),
			"There is no safeslice.yaml here yet. Create one by running the wizard, "+
				"or generate one from your schema.",
			"safeslice init --from \"postgres://...\"")
	}
	dsn, err := sourceDSN("")
	if err != nil {
		dsn = ui.Ask("\nSource connection string:", "")
		if dsn == "" {
			return errors.New("no source given")
		}
	}
	conn, err := connectSource(ctx, dsn)
	if err != nil {
		return ui.Hint(err, causesFor(err))
	}
	defer conn.Close(context.Background())

	ui.Info("source %s (read-only session)", describe(dsn))
	cat, err := catalog.Load(ctx, conn, cfg.Source.Schemas)
	if err != nil {
		return err
	}
	return renderPlan(cat, cfg)
}

// verifyFlow scans a database somebody already has.
func verifyFlow(ctx context.Context) error {
	ui.Section("Verify")
	ui.Detail("Scans a database for values that still look like real personal data:")
	ui.Detail("live email addresses, phone numbers, IP addresses and card numbers.")
	target := ui.Ask("\nDatabase to scan:", "postgres://localhost:5432/myapp_dev")
	if target == "" {
		return errors.New("no database given")
	}
	err := doVerify(ctx, target, 1000, nil)
	if err == nil {
		ui.Detail("A clean scan means no known pattern of personal data was found in")
		ui.Detail("the sampled rows. It is not proof that none exists.")
	}
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// label returns the label of a selected option, or "" when nothing was chosen.
func label(opts []ui.Option, i int) string {
	if i < 0 || i >= len(opts) {
		return ""
	}
	return opts[i].Label
}
