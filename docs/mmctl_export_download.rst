.. _mmctl_export_download:

mmctl export download
---------------------

Download export files

Synopsis
~~~~~~~~


Download export files

::

  mmctl export download [exportname] [filepath] [flags]

Examples
~~~~~~~~

::

    # you can indicate the name of the export and its destination path
    $ mmctl export download samplename sample_export.zip
    
    # or if you only indicate the name, the path would match it
    $ mmctl export download sample_export.zip

Options
~~~~~~~

::

  -h, --help     help for download
      --resume   Set to true to resume an export download.

Options inherited from parent commands
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

::

      --config string                path to the configuration file (default "$XDG_CONFIG_HOME/mmctl/config")
      --disable-pager                disables paged output
      --insecure-sha1-intermediate   allows to use insecure TLS protocols, such as SHA-1
      --insecure-tls-version         allows to use TLS versions 1.0 and 1.1
      --json                         the output format will be in json format
      --local                        allows communicating with the server through a unix socket
      --quiet                        prevent mmctl to generate output for the commands
      --strict                       will only run commands if the mmctl version matches the server one
      --suppress-warnings            disables printing warning messages

SEE ALSO
~~~~~~~~

* `mmctl export <mmctl_export.rst>`_ 	 - Management of exports

