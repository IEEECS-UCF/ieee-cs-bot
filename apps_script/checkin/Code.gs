// IEEE CS @ UCF event check-in, deployed as a web app on the chapter Google
// account. The Discord bot calls it (with the shared secret) to create one
// check-in form per event and to read back attendance counts.
//
// Setup: run setup() once from the editor; it creates the responses
// spreadsheet and the secret, and logs the secret for the bot's
// CHECKIN_SECRET. Then deploy as a web app (execute as me, anyone can access).

const FOLDER_NAME = 'IEEE CS Check-ins';
const SPREADSHEET_NAME = 'IEEE CS Check-ins (responses)';
const HEARD_OPTIONS = [
  'Discord', 'Instagram', 'Flyer or poster', 'KnightConnect',
  'A friend', 'Class or professor', 'LinkedIn', 'Other',
];
const Q = {
  name: 'Name',
  email: 'Email',
  member: 'Are you an IEEE member?',
  number: 'IEEE member number (optional)',
  heard: 'How did you hear about this event?',
};

function setup() {
  const props = PropertiesService.getScriptProperties();
  if (!props.getProperty('SECRET')) {
    props.setProperty('SECRET', Utilities.getUuid() + Utilities.getUuid());
  }
  getSpreadsheet();
  Logger.log('CHECKIN_SECRET=' + props.getProperty('SECRET'));
}

function doPost(e) {
  let req;
  try {
    req = JSON.parse(e.postData.contents);
  } catch (err) {
    return reply({error: 'bad request'});
  }
  const secret = PropertiesService.getScriptProperties().getProperty('SECRET');
  if (!secret || req.secret !== secret) return reply({error: 'unauthorized'});
  try {
    if (req.action === 'ping') return reply({ok: true});
    if (req.action === 'create') return reply(createForm(req.event, req.date, req.key));
    if (req.action === 'results') return reply(results(req.formId));
    if (req.action === 'info') return reply(formInfo(FormApp.openById(req.formId), true));
    return reply({error: 'unknown action'});
  } catch (err) {
    return reply({error: String(err)});
  }
}

function reply(obj) {
  return ContentService.createTextOutput(JSON.stringify(obj))
      .setMimeType(ContentService.MimeType.JSON);
}

// One form per event; asking again for the same key returns the same form.
function createForm(event, date, key) {
  if (!event) throw new Error('missing event name');
  const props = PropertiesService.getScriptProperties();
  const title = 'Check-in: ' + event + (date ? ' (' + date + ')' : '');
  const existing = key && props.getProperty('form:' + key);
  if (existing) {
    const form = FormApp.openById(existing);
    if (!form.getTitle()) form.setTitle(title);
    return formInfo(form, true);
  }

  // The file name and the title people see are separate in Forms
  const form = FormApp.create(title);
  form.setTitle(title);
  form.setDescription('Welcome to ' + event + '! Check in so we can count attendance. It takes 20 seconds.')
      .setCollectEmail(false)
      .setAllowResponseEdits(false)
      .setShowLinkToRespondAgain(false)
      .setConfirmationMessage("You're checked in. Enjoy the event!");
  form.addTextItem().setTitle(Q.name).setRequired(true);
  form.addTextItem().setTitle(Q.email).setHelpText('UCF email preferred').setRequired(true)
      .setValidation(FormApp.createTextValidation().requireTextIsEmail().build());
  form.addMultipleChoiceItem().setTitle(Q.member)
      .setChoiceValues(['Yes', 'No', 'Not sure']).setRequired(true);
  form.addTextItem().setTitle(Q.number)
      .setValidation(FormApp.createTextValidation().requireNumber().build());
  form.addCheckboxItem().setTitle(Q.heard).setChoiceValues(HEARD_OPTIONS);

  form.setDestination(FormApp.DestinationType.SPREADSHEET, getSpreadsheet().getId());
  DriveApp.getFileById(form.getId()).moveTo(getFolder());
  if (key) props.setProperty('form:' + key, form.getId());
  return formInfo(form, false);
}

function formInfo(form, existed) {
  const url = form.getPublishedUrl();
  let shortUrl = url;
  try {
    shortUrl = form.shortenFormUrl(url);
  } catch (err) {
    // Short links are optional
  }
  return {formId: form.getId(), title: form.getTitle(), url: url, shortUrl: shortUrl, existed: existed};
}

// Which question is which, by keyword, so forms made by hand work too.
function questionKind(title) {
  const t = String(title).toLowerCase();
  if (t.indexOf('email') !== -1) return 'email';
  if (t.indexOf('ieee') !== -1 && t.indexOf('member') !== -1 &&
      t.indexOf('number') === -1 && t.indexOf('#') === -1) return 'member';
  if (t.indexOf('hear') !== -1) return 'heard';
  return '';
}

// Counts each email once. "Not sure" members count as guests for vTools.
function results(formId) {
  if (!formId) throw new Error('missing formId');
  const form = FormApp.openById(formId);
  const seen = {};
  const heard = {};
  let total = 0, ieee = 0, notSure = 0;
  form.getResponses().forEach(function (response) {
    let email = '', member = '', sources = [];
    response.getItemResponses().forEach(function (item) {
      const kind = questionKind(item.getItem().getTitle());
      const value = item.getResponse();
      if (kind === 'email') email = String(value).trim().toLowerCase();
      else if (kind === 'member') member = String(value).trim().toLowerCase();
      else if (kind === 'heard') sources = Array.isArray(value) ? value : [value];
    });
    if (!email) email = String(response.getRespondentEmail() || '').trim().toLowerCase();
    if (email) {
      if (seen[email]) return;
      seen[email] = true;
    }
    total++;
    if (member.indexOf('yes') === 0) ieee++;
    if (member.indexOf('not sure') === 0) notSure++;
    sources.forEach(function (s) { heard[s] = (heard[s] || 0) + 1; });
  });
  return {title: form.getTitle(), total: total, ieee: ieee, guests: total - ieee, notSure: notSure, heard: heard};
}

function getFolder() {
  const props = PropertiesService.getScriptProperties();
  const id = props.getProperty('FOLDER_ID');
  if (id) return DriveApp.getFolderById(id);
  const folder = DriveApp.createFolder(FOLDER_NAME);
  props.setProperty('FOLDER_ID', folder.getId());
  return folder;
}

function getSpreadsheet() {
  const props = PropertiesService.getScriptProperties();
  const id = props.getProperty('SPREADSHEET_ID');
  if (id) return SpreadsheetApp.openById(id);
  const ss = SpreadsheetApp.create(SPREADSHEET_NAME);
  DriveApp.getFileById(ss.getId()).moveTo(getFolder());
  props.setProperty('SPREADSHEET_ID', ss.getId());
  return ss;
}
