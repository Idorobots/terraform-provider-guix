;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix secrets)
  #:use-module (guix records)
  #:use-module ((guix build utils) #:select (with-directory-excursion))
  #:use-module (guix-secrets files)
  #:use-module (rnrs bytevectors)
  #:use-module (srfi srfi-1)
  #:export (secret-file secret-file? secret-file-source secret-file-content
            secret-file-destination secret-file-mode
            secrets-configuration secrets-configuration? secrets-configuration-files
            load-secrets))

(define-record-type* <secret-file> secret-file make-secret-file secret-file?
  (source secret-file-source (default #f))
  (content secret-file-content (default #f))
  (destination secret-file-destination)
  (mode secret-file-mode (default #o600)))

(define-record-type* <secrets-configuration>
  secrets-configuration make-secrets-configuration secrets-configuration?
  (files secrets-configuration-files (default '())))

(define* (load-secrets file #:optional directory)
  "Evaluate FILE locally. Relative payload sources are relative to FILE."
  (let* ((file (canonicalize-path file))
         (directory (or directory (dirname file)))
         (config (with-directory-excursion directory
                   ;; load* forces compilation and reports user source on errors.
                   ;; Private declarations must remain interpreted and uncached.
                   (let ((compile? %load-should-auto-compile))
                     (dynamic-wind
                       (lambda () (set! %load-should-auto-compile #f))
                       (lambda ()
                         (save-module-excursion
                          (lambda ()
                            (set-current-module (make-fresh-user-module))
                            (module-use! (current-module) (resolve-interface '(guix secrets)))
                            (primitive-load file))))
                       (lambda () (set! %load-should-auto-compile compile?)))))))
    (unless (secrets-configuration? config)
      (error "secrets declaration must return a secrets-configuration"))
    (validate-payload
     (map (lambda (entry)
            (unless (secret-file? entry) (error "expected a secret-file record"))
            (let ((source (secret-file-source entry))
                  (content (secret-file-content entry)))
              (unless (and (if source (not content) content)
                           (or (not source) (string? source)))
                (error "secret-file requires exactly one of source or content"))
              (list (secret-file-destination entry) (secret-file-mode entry)
                    (if source
                        (let ((path (if (string-prefix? "/" source) source
                                        (string-append directory "/" source))))
                          (unless (eq? 'regular (stat:type (stat path)))
                            (error "secret source must be a regular file"))
                          (read-bytes path))
                        (cond ((string? content) (string->utf8 content))
                              ((bytevector? content) content)
                              (else (error "secret content must be a string or bytevector")))))))
          (secrets-configuration-files config)))))
