package members

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

const officer, workshops = "1449921163248533525", "1424671495560626216"

var day = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

// fakeIx stands in for an interaction (FakeResponse in the Python tests).
type fakeIx struct {
	user   *discordgo.User
	member *discordgo.Member
	fields map[string]string
	sent   string
	edited *botkit.Edit
	modal  *botkit.Modal
	sends  []string
}

func interactionWith(roles ...string) *fakeIx {
	user := &discordgo.User{ID: "99", Username: "officer"}
	return &fakeIx{user: user, member: &discordgo.Member{User: user, Roles: roles}, fields: map[string]string{}}
}

func (f *fakeIx) User() *discordgo.User     { return f.user }
func (f *fakeIx) Member() *discordgo.Member { return f.member }
func (f *fakeIx) GuildID() string           { return "1" }
func (f *fakeIx) Field(id string) string    { return f.fields[id] }
func (f *fakeIx) Ephemeral(text string) error {
	f.sent = text
	f.sends = append(f.sends, text)
	return nil
}
func (f *fakeIx) Update(e botkit.Edit) error  { f.edited = &e; return nil }
func (f *fakeIx) Modal(m *botkit.Modal) error { f.modal = m; return nil }
func (f *fakeIx) Send(r botkit.Reply) (*botkit.Msg, error) {
	f.sends = append(f.sends, r.Content)
	return nil, nil
}

func (f *fakeIx) as(id string) *fakeIx {
	f.user.ID = id
	return f
}

type fakeDiscord struct {
	member *discordgo.Member
	added  []int64
	cards  []string
}

func testCog(t *testing.T) (*Cog, *fakeDiscord) {
	t.Helper()
	testutil.Setup(t)
	b, err := botkit.New("test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDiscord{}
	c := newCog(b)
	c.member = func(string, int64) (*discordgo.Member, error) { return fake.member, nil }
	c.addRole = func(_ string, _ int64, role int64, _ string) error {
		fake.added = append(fake.added, role)
		return nil
	}
	c.reviewChannel = func() string { return "" }
	c.postCard = func(channel string, e *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
		fake.cards = append(fake.cards, e.Description)
		return &discordgo.Message{ID: "6", ChannelID: channel}, nil
	}
	return c, fake
}

func TestReviewCardShowsTheDecision(t *testing.T) {
	testutil.Setup(t)
	record := members.Submit(10, "00000001", "", day)
	if e := reviewEmbed(10, record); !strings.Contains(e.Description, "Waiting") || e.Color != botkit.IEEEGold {
		t.Errorf("%q", e.Description)
	}
	record = members.Decide(10, "rejected", 99, "expired", day)
	if e := reviewEmbed(10, record); !strings.Contains(e.Description, "Rejected by <@99>: expired") {
		t.Errorf("%q", e.Description)
	}
}

func TestReviewButtonsKeepTheirCustomIDs(t *testing.T) {
	row := reviewView(10)[0].(discordgo.ActionsRow)
	approve, reject := row.Components[0].(*discordgo.Button), row.Components[1].(*discordgo.Button)
	if approve.CustomID != "member:approve:10" || reject.CustomID != "member:reject:10" ||
		approve.Style != discordgo.SuccessButton || reject.Style != discordgo.DangerButton {
		t.Errorf("%+v %+v", approve, reject)
	}
}

// ---------- buttons ----------

func TestOnlyOfficersCanReview(t *testing.T) {
	c, _ := testCog(t)
	members.Submit(10, "00000001", "", day)
	ix := interactionWith(workshops)
	c.onReview(ix, "approve", 10)
	if ix.sent != "Only officers can review memberships." || members.Get(10).Status != "pending" {
		t.Errorf("%q %q", ix.sent, members.Get(10).Status)
	}
}

func TestApproveGivesTheRole(t *testing.T) {
	c, fake := testCog(t)
	members.Submit(10, "00000001", "", day)
	fake.member = &discordgo.Member{User: &discordgo.User{ID: "10"}}
	ix := interactionWith(officer)
	c.onReview(ix, "approve", 10)
	if len(fake.added) != 1 || fake.added[0] != DuesPayingRoleID {
		t.Errorf("added %v", fake.added)
	}
	if r := members.Get(10); r.Status != "verified" || r.ReviewedBy == nil || *r.ReviewedBy != 99 {
		t.Errorf("%+v", r)
	}
	if ix.edited == nil || ix.edited.Components == nil || len(*ix.edited.Components) != 0 {
		t.Error("the buttons weren't removed")
	}
	again := interactionWith(officer)
	c.onReview(again, "approve", 10)
	if again.sent != "This request was already handled." || len(fake.added) != 1 {
		t.Errorf("%q %v", again.sent, fake.added)
	}
}

func TestApproveSomeoneWhoLeft(t *testing.T) {
	c, fake := testCog(t)
	members.Submit(10, "00000001", "", day)
	ix := interactionWith(officer)
	c.onReview(ix, "approve", 10)
	if len(fake.added) != 0 || members.Get(10).Status != "verified" ||
		len(ix.sends) != 1 || ix.sends[0] != "They're no longer in the server, so no role was given." {
		t.Errorf("%v %v", fake.added, ix.sends)
	}
}

func TestApproveWhenTheRoleCantBeGiven(t *testing.T) {
	c, fake := testCog(t)
	members.Submit(10, "00000001", "", day)
	fake.member = &discordgo.Member{User: &discordgo.User{ID: "10"}}
	c.addRole = func(string, int64, int64, string) error { return errors.New("403 Forbidden") }
	ix := interactionWith(officer)
	c.onReview(ix, "approve", 10)
	if ix.sent != "Couldn't give the role (403 Forbidden). Nothing was changed." || members.Get(10).Status != "pending" {
		t.Errorf("%q", ix.sent)
	}
}

// Discord failing to look the member up leaves the request pending, so an
// officer can press Approve again.
func TestApproveWhenDiscordCantLookThemUp(t *testing.T) {
	c, fake := testCog(t)
	members.Submit(10, "00000001", "", day)
	c.member = func(string, int64) (*discordgo.Member, error) { return nil, errors.New("503 Service Unavailable") }
	ix := interactionWith(officer)
	c.onReview(ix, "approve", 10)
	if !strings.HasPrefix(ix.sent, "Couldn't look them up on Discord (503 Service Unavailable)") ||
		members.Get(10).Status != "pending" || len(fake.added) != 0 || ix.edited != nil {
		t.Errorf("%q %s", ix.sent, members.Get(10).Status)
	}
}

func TestRejectAsksForAReason(t *testing.T) {
	c, _ := testCog(t)
	members.Submit(10, "00000001", "", day)
	ix := interactionWith(officer)
	c.onReview(ix, "reject", 10)
	if ix.modal == nil || ix.modal.Title != "Reject membership" || members.Get(10).Status != "pending" {
		t.Errorf("%+v", ix.modal)
	}
	in := ix.modal.Inputs[0]
	if !in.Optional || in.MaxLength != 200 || in.Label != "Reason (the member sees this in /member status)" {
		t.Errorf("%+v", in)
	}
	ix.fields["reason"] = "number not found"
	c.onReject(ix, 10)
	if r := members.Get(10); r.Status != "rejected" || r.Reason != "number not found" || ix.edited == nil {
		t.Errorf("%+v", r)
	}
}

func TestMemberCommandsAreOpenToEveryone(t *testing.T) {
	cmd := command()
	var names []string
	for _, o := range cmd.Options {
		names = append(names, cmd.Name+" "+o.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "member status,member verify" || cmd.DefaultMemberPermissions != nil {
		t.Errorf("%v", names)
	}
}

// ---------- people who already have the role ----------

func TestStatusRecognizesExistingRoleHolders(t *testing.T) {
	c, _ := testCog(t)
	ix := interactionWith(botkit.S(DuesPayingRoleID)).as("30")
	c.status(ix)
	if !strings.HasPrefix(ix.sent, "✅ Verified") || members.Get(30).Source != "role" {
		t.Errorf("%q", ix.sent)
	}
	ix = interactionWith(botkit.S(DuesPayingRoleID)).as("31")
	c.verify(ix)
	if !strings.Contains(ix.sent, "already verified") || ix.modal != nil {
		t.Errorf("%q", ix.sent)
	}
}

func TestRoleAddedByHandIsRecorded(t *testing.T) {
	c, _ := testCog(t)
	user := &discordgo.User{ID: "40"}
	before := &discordgo.Member{User: user}
	after := &discordgo.Member{User: user, Roles: []string{botkit.S(DuesPayingRoleID)}}
	c.onMemberUpdate(before, after)
	if members.Get(40).Status != "verified" {
		t.Error("not verified")
	}
	c.onMemberUpdate(after, before)
	if members.Get(40).Status != "rejected" {
		t.Error("not withdrawn")
	}
}

func TestRoleHoldersAreRecordedAtStartup(t *testing.T) {
	c, _ := testCog(t)
	role := []string{botkit.S(DuesPayingRoleID)}
	c.onChunk("1", []*discordgo.Member{{User: &discordgo.User{ID: "60"}, Roles: role}, {User: &discordgo.User{ID: "61"}}}, 2)
	c.onChunk("1", []*discordgo.Member{{User: &discordgo.User{ID: "62"}, Roles: role}}, 2)
	if members.Get(60) == nil || members.Get(61) != nil || members.Get(62) == nil || len(c.chunks) != 0 {
		t.Errorf("%v %v %v", members.Get(60), members.Get(61), members.Get(62))
	}
}

func TestStatusTellsLastYearsMembersToVerifyAgain(t *testing.T) {
	c, _ := testCog(t)
	members.Recognize(50, false, time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC))
	ix := interactionWith(botkit.S(DuesPayingRoleID)).as("50")
	c.status(ix)
	if !strings.Contains(ix.sent, "use `/member verify` for") {
		t.Errorf("%q", ix.sent)
	}
}

func TestRejectAfterSomeoneApprovedChangesNothing(t *testing.T) {
	c, _ := testCog(t)
	members.Submit(10, "00000001", "", day)
	modal := c.rejectModal(10)
	members.Decide(10, "verified", 77, "", day) // another officer approved meanwhile
	ix := interactionWith(officer)
	ix.fields["reason"] = "nope"
	c.onReject(ix, 10)
	if members.Get(10).Status != "verified" || !strings.Contains(ix.sent, "already handled") || modal.Title != "Reject membership" {
		t.Errorf("%q", ix.sent)
	}
}

// ---------- the verify form ----------

func TestVerifyFormPostsAReviewCard(t *testing.T) {
	c, fake := testCog(t)
	ix := interactionWith().as("70")
	c.verify(ix)
	if ix.modal == nil || ix.modal.Title != "Verify your IEEE membership" || len(ix.modal.Inputs) != 2 {
		t.Fatalf("%+v", ix.modal)
	}
	ix.fields["number"] = "12-345678"
	c.onVerify(ix)
	if !strings.HasPrefix(ix.sent, "That doesn't look like an IEEE member number") || members.Get(70) != nil {
		t.Errorf("%q", ix.sent)
	}
	c.reviewChannel = func() string { return "5" }
	ix.fields["number"], ix.fields["email"] = " 1234567 ", " a@b.c "
	c.onVerify(ix)
	r := members.Get(70)
	if r == nil || r.NumberText() != "01234567" || r.Email != "a@b.c" || r.Status != "pending" {
		t.Fatalf("%+v", r)
	}
	if !strings.HasPrefix(ix.sent, "Thanks!") || len(fake.cards) != 1 || !strings.Contains(fake.cards[0], "<@70>") {
		t.Errorf("%q %v", ix.sent, fake.cards)
	}
	if r.Card == nil || r.Card.Channel != 5 || r.Card.Message != 6 {
		t.Errorf("%+v", r.Card)
	}
	again := interactionWith().as("70")
	c.verify(again)
	if again.sent != "Your request is already waiting for an officer. 🙂" || again.modal != nil {
		t.Errorf("%q", again.sent)
	}
}
