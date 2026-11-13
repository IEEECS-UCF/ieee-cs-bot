package officer

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Nothing the bot posts publicly goes out until an officer has looked at it,
// and never on the say-so of the person who wrote it. A post waiting for that
// lives in the event's record (store.Review):
//
//	record.Reviews[kind] = {
//	    "text": what will be posted,
//	    "editor": user id of whoever wrote or last edited it (null: the bot wrote it),
//	    "ai": the editor started from a "Draft with AI" draft (kept, never shown),
//	    "status": "pending" | "approved" | "posted" | "missed" | "cancelled" (with "why"),
//	    "approver": user id, "posted": jump url,
//	    "message": [channel id, message id],  // the review message in #quality-assurance
//	    "name": the event's name, "for": event start (reminders), so a moved event gets a fresh review,
//	    "flyer": the calendar's flyer link, "has_flyer": whether an image is attached to the review
//	             message (approving an announcement posts that same attachment with the text),
//	    "image": the attachment's file name,
//	    "emergency": the reason, when an officer posted it right away without review,
//	}
//
// and as a message with Approve / Edit buttons in #quality-assurance (never in
// the event's thread). General announcements (/officer announce) aren't tied to
// an event: they're stored under a "general_<id>" key and reviewed there too.
// Announcements post as soon as they're approved. Day-of reminders post at
// REMINDER_HOUR on the day, and only if approved by then (or as soon as they are,
// before the event starts).

var Kinds = map[string]string{"announcement": "Announcement", "reminder": "Day-of reminder", "general": "General announcement"}

const (
	FlyerFile = "flyer.png"

	Pending, Approved, Posted, Missed, Cancelled = "pending", "approved", "posted", "missed", "cancelled"
)

// PostsNow: approving these posts right away.
var PostsNow = map[string]bool{"announcement": true, "general": true}

// Discord calls, swapped out in tests.
var (
	sendMessage = func(b *botkit.Bot, channelID string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		return b.S.ChannelMessageSendComplex(channelID, m)
	}
	editMessage = func(b *botkit.Bot, channelID, messageID string, e botkit.Edit) error {
		return b.ChannelMsg(channelID, messageID).Edit(e)
	}
	getChannel = func(b *botkit.Bot, id string) *discordgo.Channel { return b.Channel(id) }
)

var roleMention = regexp.MustCompile(`<@&(\d+)>`)

// MentionsIn allows exactly the mentions written into an approved post, nothing else.
func MentionsIn(text string) *discordgo.MessageAllowedMentions {
	allowed := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	if strings.Contains(text, "@everyone") || strings.Contains(text, "@here") { // Discord's "everyone" covers both
		allowed.Parse = append(allowed.Parse, discordgo.AllowedMentionTypeEveryone)
	}
	for _, m := range roleMention.FindAllStringSubmatch(text, -1) {
		allowed.Roles = append(allowed.Roles, m[1])
	}
	return allowed
}

// guildRoles is the server's roles (swapped in tests).
var guildRoles = func(b *botkit.Bot) []*discordgo.Role {
	if b == nil || b.S == nil || b.S.State == nil {
		return nil
	}
	g, err := b.S.State.Guild(botkit.GuildID())
	if err != nil {
		return nil
	}
	return g.Roles
}

// WithRoleMentions turns a role typed by name in a post ("@workshops", any
// case) into the role's mention, so it pings when it posts. Discord only does
// that in its own message box, not in the bot's text boxes. Longer names go
// first ("@Workshop Leads" before "@Workshop"); @everyone, @here and bot roles
// stay as they are.
func WithRoleMentions(b *botkit.Bot, text string) string {
	roles := slices.Clone(guildRoles(b))
	slices.SortFunc(roles, func(x, y *discordgo.Role) int { return len(y.Name) - len(x.Name) })
	for _, r := range roles {
		name := strings.TrimPrefix(r.Name, "@")
		if r.Managed || r.ID == botkit.GuildID() || name == "" || strings.EqualFold(name, "everyone") || strings.EqualFold(name, "here") {
			continue
		}
		re := regexp.MustCompile(`(?i)(^|[^\w<&@])@` + regexp.QuoteMeta(name) + `($|[^\w])`)
		text = re.ReplaceAllString(text, "${1}<@&"+r.ID+">${2}")
	}
	return text
}

// WithPing is the text an announcement posts as. Officers often rewrite the
// draft and drop its opening ping, so an announcement that mentions nobody
// opens with @everyone on its own line. Other kinds post as written.
func WithPing(kind, text string) string {
	if kind != "announcement" || strings.Contains(text, "@everyone") || roleMention.MatchString(text) {
		return text
	}
	return "@everyone\n" + text
}

// PingNote tells reviewers when WithPing adds the @everyone.
func PingNote(kind, text string) string {
	if WithPing(kind, text) == text {
		return ""
	}
	return "\nIt pings nobody as written, so it posts with @everyone on the first line."
}

// TooLong is why a post can't go out as written ("" if it fits), counting
// the @everyone WithPing adds.
func TooLong(kind, text string) string {
	n := len([]rune(WithPing(kind, text)))
	if n <= eventtext.MaxPost {
		return ""
	}
	if WithPing(kind, text) != text {
		return fmt.Sprintf("With the @everyone it opens with, it's %d characters, and Discord posts at most %d. "+
			"Shorten it and send it again.", n, eventtext.MaxPost)
	}
	return fmt.Sprintf("It's %d characters, and Discord posts at most %d. Shorten it and send it again.", n, eventtext.MaxPost)
}

// TargetChannelID is where an approved post goes (0 while posting is turned off).
func TargetChannelID(kind string) int64 {
	value := os.Getenv("REMINDER_CHANNEL_ID")
	if PostsNow[kind] && os.Getenv("ANNOUNCE_CHANNEL_ID") != "" {
		value = os.Getenv("ANNOUNCE_CHANNEL_ID")
	}
	return botkit.ID(strings.TrimSpace(value))
}

// ---------- stored reviews ----------

func GetReview(gid, kind string) *store.Review {
	return store.EventStore{}.Get(gid).Reviews[kind]
}

// PutReview stores the review; nil removes it.
func PutReview(gid, kind string, r *store.Review) error {
	_, err := store.EventStore{}.SetReview(gid, kind, r)
	return err
}

// ChangeReview is store.EventStore.ChangeReview.
func ChangeReview(gid, kind string, change func(current *store.Review) *store.Review) (bool, error) {
	return store.EventStore{}.ChangeReview(gid, kind, change)
}

// While an approval is posting (it can take a while with a flyer), the
// review is claimed: a second Approve or an edit waits for it to finish.
var (
	claimsMu sync.Mutex
	claims   = map[string]bool{}
)

// ClaimReview claims a review for posting; ok is false if it's already
// claimed. Call release when done.
func ClaimReview(gid, kind string) (release func(), ok bool) {
	key := gid + "|" + kind
	claimsMu.Lock()
	defer claimsMu.Unlock()
	if claims[key] {
		return nil, false
	}
	claims[key] = true
	return func() {
		claimsMu.Lock()
		delete(claims, key)
		claimsMu.Unlock()
	}, true
}

// Claimed is whether a review is being posted right now.
func Claimed(gid, kind string) bool {
	claimsMu.Lock()
	defer claimsMu.Unlock()
	return claims[gid+"|"+kind]
}

// CanApprove is (ok, why not).
func CanApprove(r *store.Review, userID int64) (bool, string) {
	if r == nil || r.Status != Pending {
		return false, "This isn't waiting for approval anymore."
	}
	if r.Editor != nil && *r.Editor == userID {
		return false, "You wrote or last edited this, so another officer has to approve it."
	}
	return true, ""
}

// Edited is the review after an officer changes the text: their version now
// needs someone else's approval.
func Edited(r *store.Review, text string, userID int64) *store.Review {
	c := *r
	c.Text, c.Editor, c.Status, c.Approver, c.AI = text, &userID, Pending, nil, false
	return &c
}

// ---------- the review message ----------

// StatusLine is who wrote it and what has to happen next. when: the
// reminder's post time, as text ("" for none).
func StatusLine(kind string, r *store.Review, when string) string {
	where := "the announcements channel"
	if id := TargetChannelID(kind); id != 0 {
		where = fmt.Sprintf("<#%d>", id)
	}
	by := "the bot, from the calendar"
	if r.Editor != nil && *r.Editor != 0 {
		by = fmt.Sprintf("<@%d>", *r.Editor)
	}
	at := ""
	if when != "" {
		at = " at " + when
	}
	approver := "None"
	if r.Approver != nil {
		approver = fmt.Sprint(*r.Approver)
	}
	switch r.Status {
	case Posted:
		if r.Emergency != "" {
			return fmt.Sprintf("🚨 Posted by %s without review (emergency post). Reason: %s\n%s", by, r.Emergency, r.Posted)
		}
		return fmt.Sprintf("Written by %s. Approved by <@%s> and posted: %s", by, approver, r.Posted)
	case Approved:
		return fmt.Sprintf("Written by %s. Approved by <@%s>; posts in %s%s.%s", by, approver, where, at, PingNote(kind, r.Text))
	case Missed:
		if r.Approver != nil {
			return fmt.Sprintf("Written by %s. Approved by <@%s>, but the bot couldn't post it before the event started.", by, approver)
		}
		return fmt.Sprintf("Written by %s. Nobody approved it before the event started, so it was never posted.", by)
	case Cancelled:
		why := r.Why
		if why == "" {
			why = "cancelled"
		}
		return fmt.Sprintf("Written by %s. Not posting: %s.", by, why)
	}
	who := "an officer"
	if r.Editor != nil && *r.Editor != 0 {
		who = fmt.Sprintf("an officer other than <@%d>", *r.Editor)
	}
	return fmt.Sprintf("Written by %s. Needs %s to approve it before it posts in %s%s.%s%s", by, who, where, at,
		FlyerNote(kind, r), PingNote(kind, r.Text))
}

func FlyerNote(kind string, r *store.Review) string {
	if kind != "announcement" || r.HasFlyer {
		return ""
	}
	if r.Flyer != nil && *r.Flyer != "" {
		return "\n⚠️ The flyer link isn't a downloadable image, so it won't be attached."
	}
	return "\nNo flyer on the calendar event, so it posts as text only."
}

// MessageParts is (content, embed) for the review message. The embed shows
// the post as it will look.
func MessageParts(eventName, kind string, r *store.Review, when string) (string, *discordgo.MessageEmbed) {
	title := Kinds[kind]
	if kind != "general" {
		title = fmt.Sprintf("%s for %s", Kinds[kind], eventName)
	}
	content := fmt.Sprintf("📝 **%s**\n%s", title, StatusLine(kind, r, when))
	embed := &discordgo.MessageEmbed{Description: botkit.Cut(r.Text, 4096)}
	if r.HasFlyer {
		image := r.Image
		if image == "" {
			image = FlyerFile
		}
		embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://" + image}
	}
	return content, embed
}

// ShowsText is whether a review message shows text (the embed MessageParts
// made). A message without that embed counts as showing it.
func ShowsText(m *discordgo.Message, text string) bool {
	if len(m.Embeds) == 0 {
		return true
	}
	clean := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return clean(m.Embeds[0].Description) == clean(botkit.Cut(text, 4096))
}

// ShowReview edits the review message to match the review. buttons nil
// takes the buttons off (view=None).
func ShowReview(b *botkit.Bot, kind string, r *store.Review, when string, buttons []discordgo.MessageComponent) {
	if len(r.Message) < 2 {
		return
	}
	content, embed := MessageParts(r.Name, kind, r, when)
	if buttons == nil {
		buttons = []discordgo.MessageComponent{}
	}
	err := editMessage(b, botkit.S(r.Message[0]), botkit.S(r.Message[1]), botkit.Edit{
		Content: &content, Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &buttons, Mentions: botkit.NoMentions()})
	if err != nil {
		log.Printf("Could not update the %s review for %s: %v", kind, r.Name, err)
	}
}

// ReviewOptions are open_review's optional arguments.
type ReviewOptions struct {
	Ping      int64                                               // who to ping on the review message
	When      string                                              // the reminder's post time, as text
	Buttons   func(gid, kind string) []discordgo.MessageComponent // the Approve / Edit buttons (ReviewButtons if nil)
	Extra     func(*store.Review)                                 // sets extra fields ("for", "ai")
	Flyer     []byte                                              // an image, attached so reviewers see it
	ImageName string                                              // FlyerFile if ""
}

// reviewThread is the channel review cards go in (#quality-assurance), never
// the event's thread.
func reviewThread(b *botkit.Bot, e eventtext.Event, kind string) (*discordgo.Channel, error) {
	cog, ok := b.Cog(cogapi.OfficerName).(cogapi.Officer)
	if !ok || cog == nil {
		return nil, nil
	}
	return cog.ReviewChannel()
}

func jumpURL(channel *discordgo.Channel, m *discordgo.Message) string {
	guild := m.GuildID
	if guild == "" && channel != nil {
		guild = channel.GuildID
	}
	return botkit.JumpURL(guild, m.ChannelID, m.ID)
}

// OpenReview posts a new review in #quality-assurance and stores it,
// replacing any older one. It returns the review and the review message's link, or nil and why not; err is Discord
// refusing the message.
func OpenReview(b *botkit.Bot, e eventtext.Event, kind, text string, editor *int64, o ReviewOptions) (*store.Review, string, error) {
	text = WithRoleMentions(b, text)
	if why := TooLong(kind, text); why != "" {
		return nil, why, nil
	}
	thread, err := reviewThread(b, e, kind)
	if err != nil {
		return nil, "", err
	}
	if thread == nil {
		return nil, "There's no #quality-assurance channel (or REVIEW_CHANNEL_ID), so there's nowhere to review it.", nil
	}
	if o.ImageName == "" {
		o.ImageName = FlyerFile
	}
	if o.Buttons == nil {
		o.Buttons = ReviewButtons
	}
	old := GetReview(e.ID, kind)
	r := &store.Review{Name: e.Name, Text: text, Editor: editor, Status: Pending, HasFlyer: len(o.Flyer) > 0,
		Image: o.ImageName}
	if e.Flyer != "" {
		flyer := e.Flyer
		r.Flyer = &flyer
	}
	if o.Extra != nil {
		o.Extra(r)
	}
	content, embed := MessageParts(e.Name, kind, r, o.When)
	allowed := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	if o.Ping != 0 {
		content = fmt.Sprintf("<@%d> %s", o.Ping, content)
		allowed.Users = []string{botkit.S(o.Ping)}
	}
	send := &discordgo.MessageSend{Content: content, Embeds: []*discordgo.MessageEmbed{embed},
		Components: o.Buttons(e.ID, kind), AllowedMentions: allowed}
	if len(o.Flyer) > 0 {
		send.Files = []*discordgo.File{{Name: o.ImageName, Reader: bytes.NewReader(o.Flyer)}}
	}
	message, err := sendMessage(b, thread.ID, send)
	if err != nil {
		return nil, "", err
	}
	if message.ChannelID == "" {
		message.ChannelID = thread.ID
	}
	link := jumpURL(thread, message)
	r.Message = []int64{botkit.ID(thread.ID), botkit.ID(message.ID)}
	if err := PutReview(e.ID, kind, r); err != nil {
		return nil, "", err
	}
	if old != nil && len(old.Message) >= 2 && (old.Status == Pending || old.Status == Approved) {
		content := fmt.Sprintf("📝 **%s for %s**\nReplaced by a newer version: %s", Kinds[kind], old.Name, link)
		if err := editMessage(b, botkit.S(old.Message[0]), botkit.S(old.Message[1]), botkit.Edit{Content: &content,
			Embeds: botkit.Clear[*discordgo.MessageEmbed](), Components: botkit.Clear[discordgo.MessageComponent]()}); err != nil {
			log.Printf("Could not retire the old %s review for %s: %v", kind, e.Name, err)
		}
	}
	return r, link, nil
}

// PostWithoutReview is an emergency post: it goes out now, and leaves a
// notice where it would have been reviewed, saying who posted it and why.
// It returns the post's link, or "" and why it didn't post.
func PostWithoutReview(b *botkit.Bot, e eventtext.Event, kind, text string, userID int64, reason string,
	image []byte, imageName string) (string, string) {
	if imageName == "" {
		imageName = FlyerFile
	}
	var channel *discordgo.Channel
	if id := TargetChannelID(kind); id != 0 {
		channel = getChannel(b, botkit.S(id))
	}
	if channel == nil {
		return "", "Posting announcements is turned off right now (no announcements channel set). Nothing was posted."
	}
	text = WithRoleMentions(b, text)
	post := WithPing(kind, text)
	send := &discordgo.MessageSend{Content: post, AllowedMentions: MentionsIn(post)}
	if len(image) > 0 {
		send.Files = []*discordgo.File{{Name: imageName, Reader: bytes.NewReader(image)}}
	}
	posted, err := sendMessage(b, channel.ID, send)
	if err != nil {
		return "", fmt.Sprintf("Discord didn't accept the post (%v). Nothing was posted; try again.", err)
	}
	if posted.ChannelID == "" {
		posted.ChannelID = channel.ID
	}
	link := jumpURL(channel, posted)
	old := GetReview(e.ID, kind)
	record := &store.Review{Name: e.Name, Text: text, Editor: &userID, Status: Posted, Posted: link, Emergency: reason,
		HasFlyer: len(image) > 0, Image: imageName}
	thread, err := reviewThread(b, e, kind)
	if err == nil && thread != nil {
		content, _ := MessageParts(e.Name, kind, record, "")
		var notice *discordgo.Message
		notice, err = sendMessage(b, thread.ID, &discordgo.MessageSend{Content: content, AllowedMentions: botkit.NoMentions()})
		if err == nil {
			record.Message = []int64{botkit.ID(thread.ID), botkit.ID(notice.ID)}
		}
	}
	if err != nil {
		log.Printf("Could not leave the emergency post notice for %s: %v", e.Name, err)
	}
	if err := PutReview(e.ID, kind, record); err != nil {
		log.Printf("Could not save the emergency post for %s: %v", e.Name, err)
	}
	if old != nil && len(old.Message) >= 2 && (old.Status == Pending || old.Status == Approved) {
		content := fmt.Sprintf("📝 **%s for %s**\nNot needed anymore: <@%d> made an emergency post instead: %s",
			Kinds[kind], old.Name, userID, link)
		if err := editMessage(b, botkit.S(old.Message[0]), botkit.S(old.Message[1]), botkit.Edit{Content: &content,
			Embeds: botkit.Clear[*discordgo.MessageEmbed](), Components: botkit.Clear[discordgo.MessageComponent]()}); err != nil {
			log.Printf("Could not retire the old %s review for %s: %v", kind, e.Name, err)
		}
	}
	log.Printf("EMERGENCY post for %s by user %d, skipping review. Reason: %s", e.Name, userID, reason)
	return link, ""
}

// ---------- day-of reminders ----------

// ReminderAction is what the reminder loop should do for one event right
// now: "open" a review, "post" the approved reminder, "late" (posting time
// came and it isn't approved yet: tell the owner), "missed" (the event
// started unapproved), or "".
func ReminderAction(now, start time.Time, r *store.Review, reviewFrom, postAt time.Time) string {
	if r == nil || r.For != pytime.Isoformat(start) || r.Status == Cancelled {
		if !now.Before(reviewFrom) && now.Before(start) {
			return "open"
		}
		return ""
	}
	switch r.Status {
	case Posted, Missed:
		return ""
	}
	if !now.Before(start) {
		// Pending, or approved but never posted (the bot was down at posting time)
		return "missed"
	}
	if !now.Before(postAt) && r.Status == Approved {
		return "post"
	}
	if !now.Before(postAt) && r.Status == Pending && !r.LateNoted {
		return "late"
	}
	return ""
}

// ReviewWindow is (when the review opens: reviewHour the day before, when it
// posts: postHour on the day, or an hour before the event if that's earlier,
// so a morning event still gets its reminder).
func ReviewWindow(start time.Time, reviewHour, postHour int) (time.Time, time.Time) {
	y, m, d := start.Date()
	postAt := time.Date(y, m, d, postHour, 0, 0, 0, start.Location())
	if latest := start.Add(-time.Hour); postAt.After(latest) {
		postAt = latest
	}
	return time.Date(y, m, d-1, reviewHour, 0, 0, 0, start.Location()), postAt
}

// PostAt is when an approved reminder for an event starting at start posts.
func PostAt(start time.Time) time.Time {
	_, at := ReviewWindow(start, ReviewHour(), ReminderHour())
	return at
}

// PostLabel is PostAt as "Sat Nov 21 at 8:00".
func PostLabel(start time.Time) string { return pytime.Strftime(PostAt(start), "%a %b %-d at %-H:%M") }
