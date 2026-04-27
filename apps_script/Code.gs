// GooseRelay forwarder.
//
// Apps Script web app deployed as: Execute as: Me, Access: Anyone (or Anyone with Google account).
// All traffic is AES-GCM encrypted by the client; this script is a dumb pipe
// and never sees plaintext or holds the key.
//
// Wire: client POSTs base64(encrypted batch). We forward the bytes verbatim
// to VPS_URL and return its response body verbatim.
//
// Replace VPS_URL with your server address before deploying.

const VPS_URL = 'http://YOUR.VPS.IP:8443/relay';

function doPost(e) {
  const payload = (e && e.postData && e.postData.contents) || '';
  const resp = UrlFetchApp.fetch(VPS_URL, {
    method: 'post',
    contentType: 'text/plain',
    payload: payload,
    muteHttpExceptions: true,
    followRedirects: false,
    deadline: 30,  // seconds; our long-poll window is 8s so this is plenty
  });
  return ContentService
    .createTextOutput(resp.getContentText())
    .setMimeType(ContentService.MimeType.TEXT);
}

function doGet() {
  return ContentService
    .createTextOutput('GooseRelay forwarder OK')
    .setMimeType(ContentService.MimeType.TEXT);
}
