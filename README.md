# sprocs
Analyze TSQL stored procedures

## Background

This app extracts the source tables used by stored procedures in a particular database.  You probably don't have access to the network or database in question, but you might pull some ideas from the appoach.

* The app parses the stored procedure syntax, assumed to be written in T-SQL
* [Antlr](http://www.antlr.org) was used to generate the parser from [this grammar file](https://github.com/antlr/grammars-v4/blob/master/tsql/tsql.g4)
* The [Go](https://golang.org) code queries the relevant database for procedure definitions, listens for table_source events, and sends output to CSV
* Once main.go is compiled (instructions for setting up an environment are [here](https://golang.org/doc/install), the resulting executable is run as a console application
* All library dependencies are vendored using [gvt](https://github.com/FiloSottile/gvt)
